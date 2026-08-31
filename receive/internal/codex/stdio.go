package codex

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

type stdioTransport struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	writeGate chan struct{}
	closeOnce sync.Once
	waitDone  chan struct{}
}

func startStdioTransport(binary string, extraEnv []string, logger *log.Logger) (*stdioTransport, error) {
	cmd := exec.Command(binary, "app-server", "--listen", "stdio://")
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	t := &stdioTransport{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    bufio.NewReaderSize(stdoutPipe, 64*1024),
		writeGate: make(chan struct{}, 1),
		waitDone:  make(chan struct{}),
	}
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 4096), 1024*1024)
		for scanner.Scan() {
			logger.Printf("codex: %s", scanner.Text())
		}
	}()
	go func() {
		_ = cmd.Wait()
		close(t.waitDone)
	}()
	return t, nil
}

func (t *stdioTransport) Kind() string { return "spawned-native" }

func (t *stdioTransport) ReadMessage() ([]byte, error) {
	for {
		line, err := readLineLimited(t.stdout, maxAppServerMessage)
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			return line, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func readLineLimited(reader *bufio.Reader, limit int) ([]byte, error) {
	line := make([]byte, 0, min(limit, 4*1024))
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, errors.New("app-server message exceeds 16 MiB limit")
		}
		line = append(line, fragment...)
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return line, nil
		}
		return nil, err
	}
}

func (t *stdioTransport) WriteMessage(message []byte) error {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case t.writeGate <- struct{}{}:
		defer func() { <-t.writeGate }()
	case <-t.waitDone:
		return errTransportClosed
	case <-timer.C:
		return errors.New("timed out waiting for app-server stdin writer")
	}
	select {
	case <-t.waitDone:
		return errTransportClosed
	default:
	}
	deadlineWriter, ok := t.stdin.(interface{ SetWriteDeadline(time.Time) error })
	if !ok {
		return errors.New("app-server stdin does not support write deadlines")
	}
	if err := deadlineWriter.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	defer deadlineWriter.SetWriteDeadline(time.Time{})
	if _, err := t.stdin.Write(message); err != nil {
		return err
	}
	_, err := t.stdin.Write([]byte{'\n'})
	return err
}

func (t *stdioTransport) Close() error {
	var closeErr error
	t.closeOnce.Do(func() {
		_ = t.stdin.Close()
		select {
		case <-t.waitDone:
			return
		case <-time.After(1500 * time.Millisecond):
		}
		if t.cmd.Process != nil {
			if err := syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
				closeErr = err
			}
		}
		select {
		case <-t.waitDone:
			return
		case <-time.After(2 * time.Second):
		}
		if t.cmd.Process != nil {
			_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		}
		select {
		case <-t.waitDone:
		case <-time.After(time.Second):
		}
	})
	return closeErr
}

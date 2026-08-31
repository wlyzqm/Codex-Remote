package codex

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"time"
)

const (
	websocketGUID       = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	maxWebSocketMessage = maxAppServerMessage
)

// unixWebSocket is a deliberately small RFC 6455 client for Codex's local
// Unix control socket. It supports only the text, continuation, close and
// ping/pong frames needed by app-server, keeping the receiver dependency-free.
type unixWebSocket struct {
	conn      net.Conn
	reader    *bufio.Reader
	writeMu   sync.Mutex
	closeOnce sync.Once
}

func dialUnixWebSocket(path string, timeout time.Duration) (*unixWebSocket, error) {
	conn, err := net.DialTimeout("unix", path, timeout)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*unixWebSocket, error) {
		_ = conn.Close()
		return nil, cause
	}

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fail(err)
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	_ = conn.SetDeadline(time.Now().Add(timeout))
	request := "GET / HTTP/1.1\r\n" +
		"Host: localhost\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := io.WriteString(conn, request); err != nil {
		return fail(err)
	}

	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		return fail(err)
	}
	if !strings.Contains(status, " 101 ") {
		return fail(fmt.Errorf("websocket upgrade failed: %s", strings.TrimSpace(status)))
	}
	headers := textproto.MIMEHeader{}
	tp := textproto.NewReader(reader)
	headers, err = tp.ReadMIMEHeader()
	if err != nil {
		return fail(err)
	}
	digest := sha1.Sum([]byte(key + websocketGUID))
	wantAccept := base64.StdEncoding.EncodeToString(digest[:])
	if !strings.EqualFold(headers.Get("Upgrade"), "websocket") || headers.Get("Sec-WebSocket-Accept") != wantAccept {
		return fail(errors.New("invalid websocket upgrade response"))
	}
	_ = conn.SetDeadline(time.Time{})
	return &unixWebSocket{conn: conn, reader: reader}, nil
}

func (w *unixWebSocket) Kind() string { return "shared-daemon" }

func (w *unixWebSocket) ReadMessage() ([]byte, error) {
	var assembled bytes.Buffer
	started := false
	for {
		fin, opcode, payload, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch opcode {
		case 0x8:
			return nil, io.EOF
		case 0x9:
			if err := w.writeFrame(0xA, payload); err != nil {
				return nil, err
			}
			continue
		case 0xA:
			continue
		case 0x1:
			if started {
				return nil, errors.New("unexpected text frame during fragmented message")
			}
			started = true
		case 0x0:
			if !started {
				return nil, errors.New("unexpected websocket continuation frame")
			}
		default:
			return nil, fmt.Errorf("unsupported websocket opcode %d", opcode)
		}
		if assembled.Len()+len(payload) > maxWebSocketMessage {
			return nil, errors.New("websocket message exceeds 16 MiB limit")
		}
		_, _ = assembled.Write(payload)
		if fin {
			return assembled.Bytes(), nil
		}
	}
}

func (w *unixWebSocket) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var header [2]byte
	if _, err = io.ReadFull(w.reader, header[:]); err != nil {
		return
	}
	fin = header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		err = errors.New("websocket RSV bits are not supported")
		return
	}
	opcode = header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var extended [2]byte
		if _, err = io.ReadFull(w.reader, extended[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err = io.ReadFull(w.reader, extended[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	if length > maxWebSocketMessage {
		err = errors.New("websocket frame exceeds 16 MiB limit")
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(w.reader, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, int(length))
	if _, err = io.ReadFull(w.reader, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

func (w *unixWebSocket) WriteMessage(message []byte) error {
	return w.writeFrame(0x1, message)
}

func (w *unixWebSocket) writeFrame(opcode byte, payload []byte) error {
	if len(payload) > maxWebSocketMessage {
		return errors.New("websocket message exceeds 16 MiB limit")
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	var frame bytes.Buffer
	frame.WriteByte(0x80 | opcode)
	switch length := len(payload); {
	case length < 126:
		frame.WriteByte(0x80 | byte(length))
	case length <= 0xffff:
		frame.WriteByte(0x80 | 126)
		var extended [2]byte
		binary.BigEndian.PutUint16(extended[:], uint16(length))
		frame.Write(extended[:])
	default:
		frame.WriteByte(0x80 | 127)
		var extended [8]byte
		binary.BigEndian.PutUint64(extended[:], uint64(length))
		frame.Write(extended[:])
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	frame.Write(mask[:])
	for i, value := range payload {
		frame.WriteByte(value ^ mask[i%4])
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err := w.conn.Write(frame.Bytes())
	_ = w.conn.SetWriteDeadline(time.Time{})
	return err
}

func (w *unixWebSocket) Close() error {
	var closeErr error
	w.closeOnce.Do(func() {
		_ = w.writeFrame(0x8, []byte{0x03, 0xE8})
		closeErr = w.conn.Close()
	})
	return closeErr
}

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"codex-remote/internal/auth"
	"codex-remote/internal/codex"
	"codex-remote/internal/events"
	"codex-remote/internal/policy"
	"codex-remote/internal/server"
)

var version = "0.7.1"

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	command := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command = args[0]
		args = args[1:]
	}
	var err error
	switch command {
	case "serve":
		err = runServe(args)
	case "version":
		fmt.Printf("codex-remote %s\n", version)
	default:
		err = fmt.Errorf("unknown command %q (available: serve, version)", command)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "codex-remote: %v\n", err)
		os.Exit(1)
	}
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:18787", "HTTP(S) listen address")
	configPath := flags.String("config", defaultConfigPath(), "private JSON config file containing the login password")
	webRoot := flags.String("web-root", defaultWebRoot(), "directory containing the send PWA")
	codexBin := flags.String("codex-bin", "auto", "Codex executable, or auto for native-binary discovery")
	codexHome := flags.String("codex-home", os.Getenv("CODEX_HOME"), "Codex state directory (default: ~/.codex)")
	appServerMode := flags.String("app-server-mode", "auto", "auto, daemon, or spawn")
	daemonSocket := flags.String("daemon-socket", "", "managed app-server Unix socket")
	idleTimeout := flags.Duration("idle-timeout", 2*time.Minute, "close app-server connection after safe inactivity; 0 disables")
	sessionTTL := flags.Duration("session-ttl", 30*24*time.Hour, "browser session lifetime")
	tlsCert := flags.String("tls-cert", "", "TLS certificate chain")
	tlsKey := flags.String("tls-key", "", "TLS private key")
	allowPublicHTTP := flags.Bool("allow-public-http", false, "explicitly allow cleartext HTTP on a non-loopback address")
	trustedProxy := flags.Bool("trusted-proxy", false, "trust forwarding headers from the configured TLS reverse proxy")
	var allowedRoots stringList
	flags.Var(&allowedRoots, "allow-root", "deprecated; authenticated users can access all workstation paths")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if (*tlsCert == "") != (*tlsKey == "") {
		return errors.New("--tls-cert and --tls-key must be provided together")
	}
	if !listenerSecurityConfigured(*listen, *tlsCert != "", *allowPublicHTTP, *trustedProxy) {
		return errors.New("refusing public cleartext HTTP; configure direct TLS, pass --trusted-proxy behind a TLS reverse proxy, bind loopback, or explicitly pass --allow-public-http")
	}
	paths, err := policy.NewPaths(nil)
	if err != nil {
		return err
	}
	authConfig, err := auth.LoadConfig(*configPath)
	if err != nil {
		return fmt.Errorf("load config %s: %w", *configPath, err)
	}
	sessionKey, err := auth.NewSessionKey()
	if err != nil {
		return fmt.Errorf("create session key: %w", err)
	}
	if *codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve Codex home: %w", err)
		}
		*codexHome = filepath.Join(home, ".codex")
	}
	uploadRoot := filepath.Join(filepath.Dir(*codexHome), ".cache", "codex-remote", "uploads")
	if err := os.MkdirAll(uploadRoot, 0o700); err != nil {
		return fmt.Errorf("create upload directory: %w", err)
	}
	logger := log.New(os.Stderr, "codex-remote: ", log.LstdFlags|log.Lmsgprefix)
	broker := events.New(2048, 2<<20)
	var web *server.Server
	emit := func(value any) {
		data, err := json.Marshal(value)
		if err != nil {
			logger.Printf("could not encode event: %v", err)
			return
		}
		broker.Publish(data)
		if web != nil {
			web.Observe(data)
		}
	}
	backend, err := codex.New(codex.Config{
		Mode: *appServerMode, CodexBin: *codexBin, CodexHome: *codexHome,
		DaemonSocket: *daemonSocket, IdleTimeout: *idleTimeout, Logger: logger, Emit: emit,
	})
	if err != nil {
		return err
	}
	defer backend.Close()
	web, err = server.New(server.Config{
		CodexHome: *codexHome,
		Password:  authConfig.Password, SessionKey: sessionKey, WebRoot: *webRoot, SessionTTL: *sessionTTL,
		GeneratedImagesRoot: filepath.Join(*codexHome, "generated_images"), UploadRoot: uploadRoot,
		TrustedProxy: *trustedProxy, Version: version, Logger: logger, Paths: paths,
	}, backend, broker)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           web.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	scheme := "http"
	if *tlsCert != "" {
		scheme = "https"
	}
	workspaceScope := "unrestricted (direct protected paths excluded)"
	if roots := paths.Roots(); len(roots) > 0 {
		workspaceScope = strings.Join(roots, ", ")
	}
	logger.Printf("receiver %s listening on %s://%s (workspace scope: %s)", version, scheme, *listen, workspaceScope)
	if *tlsCert != "" {
		err = httpServer.ListenAndServeTLS(*tlsCert, *tlsKey)
	} else {
		err = httpServer.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func isLoopbackListen(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func listenerSecurityConfigured(address string, directTLS, allowPublicHTTP, trustedProxy bool) bool {
	return directTLS || allowPublicHTTP || trustedProxy || isLoopbackListen(address)
}

func defaultWebRoot() string {
	if executable, err := os.Executable(); err == nil {
		candidate := filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "send"))
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		for _, candidate := range []string{filepath.Join(cwd, "..", "send"), filepath.Join(cwd, "send")} {
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate
			}
		}
	}
	return "../send"
}

func defaultConfigPath() string {
	if executable, err := os.Executable(); err == nil {
		return filepath.Clean(filepath.Join(filepath.Dir(executable), "..", "config.json"))
	}
	if cwd, err := os.Getwd(); err == nil {
		if filepath.Base(cwd) == "receive" {
			return filepath.Join(filepath.Dir(cwd), "config.json")
		}
		return filepath.Join(cwd, "config.json")
	}
	return "config.json"
}

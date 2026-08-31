package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"codex-remote/internal/auth"
	"codex-remote/internal/codex"
	"codex-remote/internal/events"
	"codex-remote/internal/policy"
)

const maxRequestBody = 12 << 20
const maxArtifactBytes = 32 << 20
const maxUploadBytes = 8 << 20
const maxProjectPreviewBytes = 2 << 20
const maxProjectEntries = 2000
const maxRevokedSessions = 4096
const maxLoginFailures = 20

type Backend interface {
	Call(context.Context, string, json.RawMessage) (json.RawMessage, *codex.RPCError, error)
	PendingRequests() []codex.PublicRequest
	Respond(context.Context, string, json.RawMessage) error
	Status() codex.Status
	AuthorizeThread(string)
}

type Config struct {
	Password            string
	SessionKey          string
	WebRoot             string
	GeneratedImagesRoot string
	SessionTTL          time.Duration
	TrustedProxy        bool
	Version             string
	Logger              *log.Logger
	Paths               *policy.Paths
}

type Server struct {
	cfg       Config
	backend   Backend
	broker    *events.Broker
	mux       *http.ServeMux
	limiter   *loginLimiter
	rpcSlots  chan struct{}
	sessionMu sync.Mutex
	revoked   map[string]time.Time
	streams   map[string]map[*sessionStream]struct{}
}

type sessionStream struct {
	done chan struct{}
	once sync.Once
}

func (s *sessionStream) close() {
	s.once.Do(func() { close(s.done) })
}

func New(cfg Config, backend Backend, broker *events.Broker) (*Server, error) {
	if cfg.Password == "" {
		return nil, errors.New("authentication password is required")
	}
	if cfg.SessionKey == "" {
		return nil, errors.New("session signing key is required")
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = 30 * 24 * time.Hour
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "codex-remote: ", log.LstdFlags)
	}
	if cfg.Paths == nil {
		return nil, errors.New("workspace path policy is required")
	}
	root, err := filepath.Abs(cfg.WebRoot)
	if err != nil {
		return nil, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("web root %q is not a directory", root)
	}
	cfg.WebRoot = root
	if cfg.GeneratedImagesRoot != "" {
		generatedRoot, err := filepath.Abs(cfg.GeneratedImagesRoot)
		if err != nil {
			return nil, err
		}
		if resolved, resolveErr := filepath.EvalSymlinks(generatedRoot); resolveErr == nil {
			generatedRoot = resolved
		} else if !errors.Is(resolveErr, os.ErrNotExist) {
			return nil, resolveErr
		}
		cfg.GeneratedImagesRoot = filepath.Clean(generatedRoot)
	}
	s := &Server{
		cfg: cfg, backend: backend, broker: broker, mux: http.NewServeMux(), limiter: newLoginLimiter(),
		rpcSlots: make(chan struct{}, 16),
		revoked:  make(map[string]time.Time), streams: make(map[string]map[*sessionStream]struct{}),
	}
	s.routes()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	return s.securityHeaders(s.mux)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("POST /api/login", s.handleLogin)
	s.mux.HandleFunc("GET /api/session", s.handleSession)
	s.mux.HandleFunc("POST /api/logout", s.requireAuth(s.handleLogout))
	s.mux.HandleFunc("GET /api/status", s.requireAuth(s.handleStatus))
	s.mux.HandleFunc("GET /api/directories", s.requireAuth(s.handleDirectories))
	s.mux.HandleFunc("POST /api/rpc", s.requireAuth(s.handleRPC))
	s.mux.HandleFunc("GET /api/events", s.requireAuth(s.handleEvents))
	s.mux.HandleFunc("GET /api/requests", s.requireAuth(s.handleRequests))
	s.mux.HandleFunc("POST /api/requests/{key}/respond", s.requireAuth(s.handleRespond))
	s.mux.HandleFunc("GET /api/artifact", s.requireAuth(s.handleArtifact))
	s.mux.HandleFunc("GET /api/files", s.requireAuth(s.handleFiles))
	s.mux.HandleFunc("POST /api/uploads", s.requireAuth(s.handleUpload))
	s.mux.HandleFunc("GET /", s.handleStatic)
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin_not_allowed", "请求来源不被允许")
		return
	}
	clientIP := s.clientIP(r)
	if !s.limiter.Allow(clientIP, time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "login_rate_limited", "尝试次数过多，请稍后再试")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !auth.EqualPassword(s.cfg.Password, body.Password) {
		s.limiter.Failure(clientIP, time.Now())
		time.Sleep(120 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, "invalid_password", "访问密码错误")
		return
	}
	s.limiter.Success(clientIP)
	expires := time.Now().Add(s.cfg.SessionTTL)
	value, err := auth.IssueSession(s.cfg.SessionKey, expires)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_error", "无法创建会话")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(r),
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.isSecure(r),
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": s.authenticated(r)})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	for _, name := range []string{"__Host-codex_remote_session", "codex_remote_dev"} {
		if cookie, err := r.Cookie(name); err == nil {
			s.revokeSession(cookie.Value, time.Now().Add(s.cfg.SessionTTL))
		}
		http.SetCookie(w, &http.Cookie{
			Name: name, Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(1, 0),
			HttpOnly: true, Secure: name == "__Host-codex_remote_session", SameSite: http.SameSiteStrictMode,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	workspaceMode := "restricted"
	if s.cfg.Paths.Unrestricted() {
		workspaceMode = "unrestricted"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"receiverVersion": s.cfg.Version,
		"backend":         s.backend.Status(),
		"allowedRoots":    s.cfg.Paths.Roots(),
		"workspaceMode":   workspaceMode,
		"eventInstanceId": s.broker.InstanceID(),
	})
}

func (s *Server) handleDirectories(w http.ResponseWriter, r *http.Request) {
	requested := r.URL.Query().Get("path")
	if requested == "" {
		requested = string(filepath.Separator)
	}
	if requested == string(filepath.Separator) && !s.cfg.Paths.Unrestricted() {
		entries := make([]map[string]string, 0, len(s.cfg.Paths.Roots()))
		for _, root := range s.cfg.Paths.Roots() {
			entries = append(entries, map[string]string{"name": root, "path": root})
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": requested, "parent": "", "entries": entries})
		return
	}
	canonical, err := s.cfg.Paths.Check(requested)
	if err != nil {
		writeError(w, http.StatusForbidden, "directory_not_allowed", "目录不可访问或不在允许范围内")
		return
	}
	items, err := os.ReadDir(canonical)
	if err != nil {
		writeError(w, http.StatusForbidden, "directory_unreadable", "无法读取这个目录")
		return
	}
	entries := make([]map[string]string, 0, len(items))
	for _, item := range items {
		if !item.IsDir() {
			continue
		}
		child := filepath.Join(canonical, item.Name())
		if _, err := s.cfg.Paths.Check(child); err == nil {
			entries = append(entries, map[string]string{"name": item.Name(), "path": child})
		}
	}
	parent := ""
	if canonical != string(filepath.Separator) {
		candidate := filepath.Dir(canonical)
		if _, err := s.cfg.Paths.Check(candidate); err == nil {
			parent = candidate
		} else if !s.cfg.Paths.Unrestricted() {
			parent = string(filepath.Separator)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": canonical, "parent": parent, "entries": entries})
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if _, ok := policy.AllowedMethods[body.Method]; !ok {
		writeError(w, http.StatusForbidden, "method_not_allowed", "该 Codex 方法未向远程端开放")
		return
	}
	select {
	case s.rpcSlots <- struct{}{}:
		defer func() { <-s.rpcSlots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusTooManyRequests, "receiver_busy", "并发 Codex 请求已达到上限")
		return
	}
	params := body.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	var err error
	if body.Method == "thread/start" || body.Method == "thread/resume" || body.Method == "thread/fork" {
		params, err = s.cfg.Paths.SanitizeLaunchParams(body.Method, params)
		if err != nil {
			writeError(w, http.StatusForbidden, "remote_policy_rejected", err.Error())
			return
		}
	} else if body.Method == "turn/start" || body.Method == "turn/steer" {
		params, err = policy.SanitizeTurnParams(body.Method, params)
		if err != nil {
			writeError(w, http.StatusForbidden, "remote_policy_rejected", err.Error())
			return
		}
	} else if body.Method == "modelProvider/capabilities/read" || body.Method == "thread/compact/start" || body.Method == "review/start" {
		params, err = policy.SanitizeRichClientParams(body.Method, params)
		if err != nil {
			writeError(w, http.StatusForbidden, "remote_policy_rejected", err.Error())
			return
		}
	} else {
		params, err = policy.SanitizeStandardClientParams(body.Method, params)
		if err != nil {
			writeError(w, http.StatusForbidden, "remote_policy_rejected", err.Error())
			return
		}
	}
	if requiresExistingThread(body.Method) {
		threadID := policy.ThreadID(params)
		if threadID == "" {
			writeError(w, http.StatusBadRequest, "thread_id_required", "缺少 threadId")
			return
		}
		if err := s.ensureThreadAllowed(r.Context(), threadID); err != nil {
			s.cfg.Logger.Printf("blocked remote access to thread %q: %v", threadID, err)
			writeError(w, http.StatusForbidden, "thread_not_allowed", "会话不在允许的工作目录内或不可用")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	result, rpcErr, callErr := s.backend.Call(ctx, body.Method, params)
	if callErr != nil {
		writeError(w, http.StatusBadGateway, "app_server_unavailable", callErr.Error())
		return
	}
	if rpcErr != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": rpcErr})
		return
	}
	if body.Method == "thread/list" {
		result, err = s.filterThreadList(result)
		if err != nil {
			s.cfg.Logger.Printf("invalid thread/list response: %v", err)
			writeError(w, http.StatusBadGateway, "invalid_app_server_response", "Codex 返回了无法验证的会话列表")
			return
		}
	}
	if body.Method == "thread/read" || body.Method == "thread/start" || body.Method == "thread/resume" || body.Method == "thread/fork" {
		if err := s.checkThreadResult(result); err != nil {
			s.cfg.Logger.Printf("blocked thread response for %s: %v", body.Method, err)
			writeError(w, http.StatusForbidden, "thread_not_allowed", "会话不在允许的工作目录内或不可用")
			return
		}
		if threadID := resultThreadID(result); threadID != "" {
			s.backend.AuthorizeThread(threadID)
		}
		if body.Method == "thread/read" {
			result = attachThreadRuntime(result)
		}
	}
	writeRawResult(w, result)
}

func requiresExistingThread(method string) bool {
	switch method {
	case "thread/resume", "thread/fork", "thread/name/set", "thread/archive", "thread/unarchive",
		"thread/goal/get", "thread/goal/set", "thread/goal/clear", "thread/compact/start", "review/start",
		"turn/start", "turn/steer", "turn/interrupt":
		return true
	default:
		return false
	}
}

func (s *Server) ensureThreadAllowed(ctx context.Context, threadID string) error {
	_, err := s.threadWorkspace(ctx, threadID)
	return err
}

func (s *Server) threadWorkspace(ctx context.Context, threadID string) (string, error) {
	if threadID == "" {
		return "", errors.New("thread id is required")
	}
	params, _ := json.Marshal(map[string]any{"threadId": threadID, "includeTurns": false})
	readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result, rpcErr, err := s.backend.Call(readCtx, "thread/read", params)
	if err != nil {
		return "", err
	}
	if rpcErr != nil {
		return "", rpcErr
	}
	var envelope struct {
		Thread struct {
			ID  string `json:"id"`
			CWD string `json:"cwd"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil || envelope.Thread.ID != threadID {
		return "", errors.New("thread response does not match the requested thread")
	}
	workspace, err := s.cfg.Paths.Check(envelope.Thread.CWD)
	if err != nil {
		return "", err
	}
	s.backend.AuthorizeThread(threadID)
	return workspace, nil
}

func resultThreadID(result json.RawMessage) string {
	var envelope struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(result, &envelope)
	return envelope.Thread.ID
}

func attachThreadRuntime(result json.RawMessage) json.RawMessage {
	var envelope map[string]any
	if json.Unmarshal(result, &envelope) != nil {
		return result
	}
	thread, ok := envelope["thread"].(map[string]any)
	if !ok {
		return result
	}
	threadID, _ := thread["id"].(string)
	rolloutPath, _ := thread["path"].(string)
	if threadID == "" || !filepath.IsAbs(rolloutPath) || filepath.Ext(rolloutPath) != ".jsonl" || !strings.Contains(filepath.Base(rolloutPath), threadID) {
		return result
	}
	file, err := os.Open(rolloutPath)
	if err != nil {
		return result
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return result
	}
	const tailLimit = int64(20 << 20)
	offset := info.Size() - tailLimit
	if offset < 0 {
		offset = 0
	}
	data := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(data, offset); err != nil && err != io.EOF {
		return result
	}
	if offset > 0 {
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		}
	}
	lines := bytes.Split(data, []byte{'\n'})
	for index := len(lines) - 1; index >= 0; index-- {
		var record struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(lines[index], &record) != nil {
			continue
		}
		var runtime struct {
			Type           string  `json:"type"`
			Model          string  `json:"model"`
			Effort         string  `json:"effort"`
			ServiceTier    *string `json:"service_tier"`
			Personality    string  `json:"personality"`
			ThreadSettings *struct {
				Model       string  `json:"model"`
				Effort      string  `json:"reasoning_effort"`
				ServiceTier *string `json:"service_tier"`
				Personality string  `json:"personality"`
			} `json:"thread_settings"`
		}
		if json.Unmarshal(record.Payload, &runtime) != nil {
			continue
		}
		if record.Type == "event_msg" && runtime.Type == "thread_settings_applied" && runtime.ThreadSettings != nil {
			runtime.Model = runtime.ThreadSettings.Model
			runtime.Effort = runtime.ThreadSettings.Effort
			runtime.ServiceTier = runtime.ThreadSettings.ServiceTier
			runtime.Personality = runtime.ThreadSettings.Personality
		} else if record.Type != "turn_context" {
			continue
		}
		if runtime.Model == "" {
			continue
		}
		serviceTier := ""
		if runtime.ServiceTier != nil {
			serviceTier = *runtime.ServiceTier
		}
		thread["runtime"] = map[string]any{
			"model": runtime.Model, "effort": runtime.Effort, "serviceTier": serviceTier, "personality": runtime.Personality,
		}
		encoded, err := json.Marshal(envelope)
		if err == nil {
			return encoded
		}
		return result
	}
	return result
}

func (s *Server) checkThreadResult(result json.RawMessage) error {
	var envelope struct {
		Thread struct {
			CWD string `json:"cwd"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		return fmt.Errorf("invalid thread response: %w", err)
	}
	if envelope.Thread.CWD == "" {
		return errors.New("thread does not report a workspace path")
	}
	_, err := s.cfg.Paths.Check(envelope.Thread.CWD)
	return err
}

func (s *Server) filterThreadList(result json.RawMessage) (json.RawMessage, error) {
	var envelope struct {
		Data            json.RawMessage `json:"data"`
		NextCursor      any             `json:"nextCursor"`
		BackwardsCursor any             `json:"backwardsCursor"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil {
		return nil, err
	}
	var threads []json.RawMessage
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, errors.New("thread/list response is missing data")
	}
	if err := json.Unmarshal(envelope.Data, &threads); err != nil {
		return nil, fmt.Errorf("invalid thread/list data: %w", err)
	}
	filtered := make([]json.RawMessage, 0, len(threads))
	seenIDs := make(map[string]struct{}, len(threads))
	for _, raw := range threads {
		var thread struct {
			ID  string `json:"id"`
			CWD string `json:"cwd"`
		}
		if json.Unmarshal(raw, &thread) == nil {
			if _, err := s.cfg.Paths.Check(thread.CWD); err == nil {
				if thread.ID != "" {
					if _, duplicate := seenIDs[thread.ID]; duplicate {
						continue
					}
					seenIDs[thread.ID] = struct{}{}
				}
				filtered = append(filtered, raw)
				// Listing is itself a cwd-validated discovery operation. Authorize
				// every returned id so child-agent events and approval requests can
				// be streamed immediately after the user opens the agent panel.
				if thread.ID != "" {
					s.backend.AuthorizeThread(thread.ID)
				}
			}
		}
	}
	encoded, err := json.Marshal(map[string]any{
		"data": filtered, "nextCursor": envelope.NextCursor, "backwardsCursor": envelope.BackwardsCursor,
	})
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming_unavailable", "服务器不支持事件流")
		return
	}
	afterID, _ := strconv.ParseUint(r.Header.Get("Last-Event-ID"), 10, 64)
	if value := r.URL.Query().Get("after"); value != "" {
		afterID, _ = strconv.ParseUint(value, 10, 64)
	}
	instanceMismatch := false
	if instance := r.URL.Query().Get("instance"); instance != "" && instance != s.broker.InstanceID() {
		instanceMismatch = true
		afterID = 0
	}
	replay, reset, live, accepted, cancel := s.broker.Subscribe(afterID)
	if !accepted {
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "too_many_streams", "实时连接数量已达到上限")
		return
	}
	reset = reset || instanceMismatch
	defer cancel()
	cookie, err := s.sessionCookie(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication_required", "需要登录")
		return
	}
	stream, registered := s.registerStream(cookie.Value, time.Now())
	if !registered {
		writeError(w, http.StatusUnauthorized, "authentication_required", "需要登录")
		return
	}
	defer s.unregisterStream(cookie.Value, stream)
	if !s.authenticated(r) {
		writeError(w, http.StatusUnauthorized, "authentication_required", "需要登录")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = fmt.Fprintf(w, "retry: 2000\ndata: {\"type\":\"hello\",\"instanceId\":%q,\"latestEventId\":%d}\n\n", s.broker.InstanceID(), s.broker.LatestID())
	if reset {
		_, _ = io.WriteString(w, "event: reset\ndata: {\"type\":\"reset\"}\n\n")
	}
	for _, event := range replay {
		select {
		case <-stream.done:
			return
		default:
		}
		if !s.authenticated(r) {
			return
		}
		writeSSE(w, event)
	}
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	authCheck := time.NewTicker(time.Second)
	defer authCheck.Stop()
	for {
		select {
		case event, ok := <-live:
			if !ok {
				return
			}
			if !s.authenticated(r) {
				return
			}
			writeSSE(w, event)
			flusher.Flush()
		case <-heartbeat.C:
			_, _ = io.WriteString(w, ": keepalive\n\n")
			flusher.Flush()
		case <-authCheck.C:
			if !s.authenticated(r) {
				return
			}
		case <-stream.done:
			return
		case <-r.Context().Done():
			return
		}
	}
}

func writeSSE(w io.Writer, event events.Event) {
	_, _ = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", event.ID, event.Data)
}

func (s *Server) handleRequests(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"data": s.backend.PendingRequests()})
}

func (s *Server) handleRespond(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Result json.RawMessage `json:"result"`
	}
	if err := decodeJSON(r, &body); err != nil || len(body.Result) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_response", "result 必须是有效 JSON")
		return
	}
	err := s.backend.Respond(r.Context(), r.PathValue("key"), body.Result)
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusConflict, "request_already_resolved", "该请求已处理或已失效")
		return
	}
	if errors.Is(err, codex.ErrInvalidServerResponse) {
		writeError(w, http.StatusBadRequest, "invalid_response", err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, "app_server_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleArtifact(w http.ResponseWriter, r *http.Request) {
	requested := r.URL.Query().Get("path")
	canonical, err := s.cfg.Paths.CheckTarget(requested)
	if err != nil {
		canonical, err = s.generatedImageTarget(requested)
	}
	if err != nil {
		writeError(w, http.StatusForbidden, "artifact_not_allowed", "文件不在允许的工作目录内")
		return
	}
	file, err := openArtifactFile(canonical)
	if err != nil {
		if errors.Is(err, errArtifactPathChanged) {
			writeError(w, http.StatusForbidden, "artifact_not_allowed", "文件路径在核验期间发生变化")
			return
		}
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusBadGateway, "artifact_unavailable", "无法读取该输出文件")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxArtifactBytes {
		writeError(w, http.StatusUnsupportedMediaType, "artifact_not_renderable", "输出文件不是可显示的小型常规文件")
		return
	}
	var header [512]byte
	read, err := io.ReadFull(file, header[:])
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		writeError(w, http.StatusBadGateway, "artifact_unavailable", "无法读取该输出文件")
		return
	}
	contentType := http.DetectContentType(header[:read])
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		writeError(w, http.StatusUnsupportedMediaType, "artifact_not_renderable", "仅允许显示 PNG、JPEG、GIF 或 WebP 图片")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "private, no-store")
	// Pin the response length to the size validated above. A concurrent append
	// cannot make ServeContent stream more than maxArtifactBytes.
	http.ServeContent(w, r, filepath.Base(canonical), info.ModTime(), io.NewSectionReader(file, 0, info.Size()))
}

func (s *Server) generatedImageTarget(requested string) (string, error) {
	if s.cfg.GeneratedImagesRoot == "" || !filepath.IsAbs(requested) {
		return "", errors.New("not a configured generated image")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(requested))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(s.cfg.GeneratedImagesRoot, canonical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("generated image is outside the configured directory")
	}
	return canonical, nil
}

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	workspace, err := s.threadWorkspace(r.Context(), r.URL.Query().Get("threadId"))
	if err != nil {
		writeError(w, http.StatusForbidden, "thread_not_allowed", "会话工作目录不可用")
		return
	}
	canonical, relative, err := s.projectPath(workspace, r.URL.Query().Get("path"))
	if err != nil {
		writeError(w, http.StatusForbidden, "file_not_allowed", "文件不在当前项目内")
		return
	}
	file, err := openArtifactFile(canonical)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		writeError(w, http.StatusForbidden, "file_unavailable", "无法读取该项目文件")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeError(w, http.StatusBadGateway, "file_unavailable", "无法读取该项目文件")
		return
	}
	if info.IsDir() {
		s.writeProjectDirectory(w, file, workspace, relative)
		return
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		writeError(w, http.StatusUnsupportedMediaType, "file_not_regular", "仅支持查看或下载常规文件")
		return
	}
	if r.URL.Query().Get("download") == "1" {
		contentType := mime.TypeByExtension(filepath.Ext(canonical))
		if contentType == "" {
			var header [512]byte
			read, _ := file.ReadAt(header[:], 0)
			contentType = http.DetectContentType(header[:read])
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filepath.Base(canonical)}))
		http.ServeContent(w, r, filepath.Base(canonical), info.ModTime(), io.NewSectionReader(file, 0, info.Size()))
		return
	}
	if info.Size() > maxProjectPreviewBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "file_too_large", "文件超过 2 MiB，请直接下载查看")
		return
	}
	data, err := io.ReadAll(io.NewSectionReader(file, 0, info.Size()))
	if err != nil {
		writeError(w, http.StatusBadGateway, "file_unavailable", "无法读取该项目文件")
		return
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		writeError(w, http.StatusUnsupportedMediaType, "file_not_text", "二进制文件不支持在线预览，请直接下载")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "inline")
	_, _ = w.Write(data)
}

func (s *Server) projectPath(workspace, requested string) (string, string, error) {
	requested = filepath.FromSlash(strings.TrimSpace(requested))
	if requested == "" {
		requested = "."
	}
	if filepath.IsAbs(requested) {
		return "", "", errors.New("project paths must be relative")
	}
	clean := filepath.Clean(requested)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", "", errors.New("project path escapes the workspace")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Join(workspace, clean))
	if err != nil {
		return "", "", err
	}
	relative, err := filepath.Rel(workspace, canonical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", errors.New("project path escapes the workspace")
	}
	if relative == "." {
		relative = ""
	}
	return canonical, filepath.ToSlash(relative), nil
}

func (s *Server) writeProjectDirectory(w http.ResponseWriter, directory *os.File, workspace, relative string) {
	items, err := directory.ReadDir(maxProjectEntries + 1)
	if err != nil && err != io.EOF {
		writeError(w, http.StatusBadGateway, "directory_unavailable", "无法读取项目目录")
		return
	}
	truncated := len(items) > maxProjectEntries
	if truncated {
		items = items[:maxProjectEntries]
	}
	entries := make([]map[string]any, 0, len(items))
	for _, item := range items {
		resolved, _, err := s.projectPath(workspace, filepath.ToSlash(filepath.Join(relative, item.Name())))
		if err != nil {
			continue
		}
		info, err := os.Stat(resolved)
		if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		entries = append(entries, map[string]any{
			"name": item.Name(), "path": filepath.ToSlash(filepath.Join(relative, item.Name())),
			"type": map[bool]string{true: "directory", false: "file"}[info.IsDir()], "size": info.Size(),
		})
	}
	parent := ""
	if relative != "" {
		parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))
		if parent == "." {
			parent = ""
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": relative, "parent": parent, "entries": entries, "truncated": truncated,
	})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		Data string `json:"data"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_upload", err.Error())
		return
	}
	name := filepath.Base(strings.ReplaceAll(strings.TrimSpace(body.Name), "\\", "/"))
	if name == "." || name == "" || len(name) > 180 || strings.IndexFunc(name, func(r rune) bool { return r < 32 }) >= 0 {
		writeError(w, http.StatusBadRequest, "invalid_upload", "文件名无效")
		return
	}
	metadata, encoded, ok := strings.Cut(body.Data, ",")
	if !ok || !strings.HasPrefix(strings.ToLower(metadata), "data:") || !strings.HasSuffix(strings.ToLower(metadata), ";base64") || base64.StdEncoding.DecodedLen(len(encoded)) > maxUploadBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_upload", "文件必须是有效的 Base64 数据且不超过 8 MiB")
		return
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) > maxUploadBytes {
		writeError(w, http.StatusBadRequest, "invalid_upload", "文件数据无效")
		return
	}
	root := filepath.Join(os.TempDir(), "codex-remote-uploads")
	if err := os.MkdirAll(root, 0o700); err != nil {
		writeError(w, http.StatusInternalServerError, "upload_unavailable", "无法创建临时上传目录")
		return
	}
	pruneUploads(root, time.Now().Add(-7*24*time.Hour))
	var token [12]byte
	if _, err := rand.Read(token[:]); err != nil {
		writeError(w, http.StatusInternalServerError, "upload_unavailable", "无法生成上传文件名")
		return
	}
	path := filepath.Join(root, hex.EncodeToString(token[:])+"-"+name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		_, err = file.Write(data)
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		_ = os.Remove(path)
		writeError(w, http.StatusInternalServerError, "upload_unavailable", "无法保存上传文件")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": name, "path": path, "size": len(data)})
}

func pruneUploads(root string, before time.Time) {
	entries, _ := os.ReadDir(root)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if info, err := entry.Info(); err == nil && info.ModTime().Before(before) {
			_ = os.Remove(filepath.Join(root, entry.Name()))
		}
	}
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	requested := strings.TrimPrefix(filepath.Clean("/"+r.URL.Path), "/")
	if requested == "." || requested == "" {
		requested = "index.html"
	}
	path := filepath.Join(s.cfg.WebRoot, filepath.FromSlash(requested))
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	relative, err := filepath.Rel(s.cfg.WebRoot, path)
	if err != nil || strings.HasPrefix(relative, "..") {
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	// The PWA owns its offline shell cache. Every network response must
	// revalidate so a CDN cannot keep an old JS/CSS bundle paired with a newer
	// index or service worker. Versioned asset URLs provide immediate busting.
	w.Header().Set("Cache-Control", "no-cache")
	if strings.HasSuffix(requested, ".webmanifest") {
		w.Header().Set("Content-Type", "application/manifest+json")
	} else if contentType := mime.TypeByExtension(filepath.Ext(path)); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeFile(w, r, path)
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.sameOrigin(r) {
			writeError(w, http.StatusForbidden, "origin_not_allowed", "请求来源不被允许")
			return
		}
		if !s.authenticated(r) {
			writeError(w, http.StatusUnauthorized, "authentication_required", "需要登录")
			return
		}
		next(w, r)
	}
}

func (s *Server) authenticated(r *http.Request) bool {
	cookie, err := s.sessionCookie(r)
	if err != nil || !auth.ValidateSession(s.cfg.SessionKey, cookie.Value, time.Now()) {
		return false
	}
	return !s.sessionRevoked(cookie.Value, time.Now())
}

func (s *Server) sessionCookie(r *http.Request) (*http.Cookie, error) {
	name := "codex_remote_dev"
	if s.isSecure(r) {
		name = "__Host-codex_remote_session"
	}
	cookie, err := r.Cookie(name)
	if err == nil || !s.trustedProxyPeer(r) {
		return cookie, err
	}
	if name == "__Host-codex_remote_session" {
		return r.Cookie("codex_remote_dev")
	}
	return r.Cookie("__Host-codex_remote_session")
}

func (s *Server) sessionRevoked(value string, now time.Time) bool {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.pruneRevokedLocked(now)
	_, revoked := s.revoked[value]
	return revoked
}

func (s *Server) revokeSession(value string, expires time.Time) {
	if value == "" {
		return
	}
	s.sessionMu.Lock()
	if s.revoked == nil {
		s.revoked = make(map[string]time.Time)
	}
	s.pruneRevokedLocked(time.Now())
	if _, exists := s.revoked[value]; !exists && len(s.revoked) >= maxRevokedSessions {
		for oldest := range s.revoked {
			delete(s.revoked, oldest)
			break
		}
	}
	s.revoked[value] = expires
	var active []*sessionStream
	for stream := range s.streams[value] {
		active = append(active, stream)
	}
	s.sessionMu.Unlock()
	for _, stream := range active {
		stream.close()
	}
}

func (s *Server) registerStream(value string, now time.Time) (*sessionStream, bool) {
	stream := &sessionStream{done: make(chan struct{})}
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	s.pruneRevokedLocked(now)
	if _, revoked := s.revoked[value]; revoked {
		return nil, false
	}
	if s.streams == nil {
		s.streams = make(map[string]map[*sessionStream]struct{})
	}
	if s.streams[value] == nil {
		s.streams[value] = make(map[*sessionStream]struct{})
	}
	s.streams[value][stream] = struct{}{}
	return stream, true
}

func (s *Server) unregisterStream(value string, stream *sessionStream) {
	s.sessionMu.Lock()
	defer s.sessionMu.Unlock()
	delete(s.streams[value], stream)
	if len(s.streams[value]) == 0 {
		delete(s.streams, value)
	}
}

func (s *Server) pruneRevokedLocked(now time.Time) {
	for value, expires := range s.revoked {
		if !now.Before(expires) {
			delete(s.revoked, value)
		}
	}
}

func (s *Server) cookieName(r *http.Request) string {
	if s.isSecure(r) {
		return "__Host-codex_remote_session"
	}
	return "codex_remote_dev"
}

func (s *Server) isSecure(r *http.Request) bool {
	return s.requestScheme(r) == "https"
}

func (s *Server) sameOrigin(r *http.Request) bool {
	fetchSite := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if fetchSite == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	originScheme := strings.ToLower(parsed.Scheme)
	if originScheme != "http" && originScheme != "https" {
		return false
	}
	originAuthority, ok := canonicalAuthority(originScheme, parsed.Host, "")
	if !ok {
		return false
	}
	wantScheme := s.requestScheme(r)
	wantAuthority, ok := s.requestAuthority(r, wantScheme)
	if ok && originScheme == wantScheme && originAuthority == wantAuthority {
		return true
	}
	return fetchSite == "same-origin"
}

func (s *Server) requestScheme(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if !s.trustedProxyPeer(r) {
		return scheme
	}
	forwarded := strings.ToLower(firstForwardedValue(r.Header.Values("X-Forwarded-Proto")))
	if forwarded == "http" || forwarded == "https" {
		return forwarded
	}
	return scheme
}

func (s *Server) requestAuthority(r *http.Request, scheme string) (string, bool) {
	authority := r.Host
	forwardedPort := ""
	if s.trustedProxyPeer(r) {
		if forwardedHost := firstForwardedValue(r.Header.Values("X-Forwarded-Host")); forwardedHost != "" {
			authority = forwardedHost
		}
		forwardedPort = firstForwardedValue(r.Header.Values("X-Forwarded-Port"))
	}
	canonical, ok := canonicalAuthority(scheme, authority, forwardedPort)
	if ok || authority == r.Host {
		return canonical, ok
	}
	return canonicalAuthority(scheme, r.Host, "")
}

func firstForwardedValue(values []string) string {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				return part
			}
		}
	}
	return ""
}

func canonicalAuthority(scheme, authority, forwardedPort string) (string, bool) {
	parsed, err := url.Parse("//" + strings.TrimSpace(authority))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "" {
		return "", false
	}
	port := parsed.Port()
	if port == "" {
		port = strings.TrimSpace(forwardedPort)
	}
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", false
		}
		if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
			port = ""
		}
	}
	if port != "" {
		return net.JoinHostPort(host, port), true
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]", true
	}
	return host, true
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
		if s.isSecure(r) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) clientIP(r *http.Request) string {
	peer := remoteIP(r)
	if !s.trustedProxyPeer(r) {
		return peer
	}
	forwarded, ok := rightmostForwardedIP(r.Header.Values("X-Forwarded-For"))
	if !ok {
		return peer
	}
	return forwarded.String()
}

func (s *Server) trustedProxyPeer(r *http.Request) bool {
	peer, ok := parseRemoteIP(r.RemoteAddr)
	return ok && (peer.IsLoopback() || s.cfg.TrustedProxy)
}

func parseRemoteIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func rightmostForwardedIP(values []string) (netip.Addr, bool) {
	if len(values) == 0 {
		return netip.Addr{}, false
	}
	parts := strings.Split(values[len(values)-1], ",")
	candidate := strings.TrimSpace(parts[len(parts)-1])
	addr, err := netip.ParseAddr(candidate)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxRequestBody {
		return fmt.Errorf("request body exceeds %d byte limit", maxRequestBody)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request body must contain exactly one JSON value")
	}
	return nil
}

func writeRawResult(w http.ResponseWriter, result json.RawMessage) {
	if len(result) == 0 {
		result = json.RawMessage(`null`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"result":`))
	_, _ = w.Write(result)
	_, _ = w.Write([]byte("}\n"))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

type loginAttempt struct {
	window time.Time
	count  int
}

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string]loginAttempt
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string]loginAttempt)}
}

func (l *loginLimiter) Allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.attempts) >= 4096 {
		for key, previous := range l.attempts {
			if now.Sub(previous.window) >= time.Minute {
				delete(l.attempts, key)
			}
		}
		if _, known := l.attempts[ip]; !known && len(l.attempts) >= 4096 {
			return false
		}
	}
	attempt, known := l.attempts[ip]
	if known && now.Sub(attempt.window) < time.Minute {
		return attempt.count < maxLoginFailures
	}
	if !known && len(l.attempts) >= 4096 {
		return false
	}
	delete(l.attempts, ip)
	return true
}

func (l *loginLimiter) Failure(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	attempt := l.attempts[ip]
	if attempt.window.IsZero() || now.Sub(attempt.window) >= time.Minute {
		attempt = loginAttempt{window: now}
	}
	attempt.count++
	l.attempts[ip] = attempt
}

func (l *loginLimiter) Success(ip string) {
	l.mu.Lock()
	delete(l.attempts, ip)
	l.mu.Unlock()
}

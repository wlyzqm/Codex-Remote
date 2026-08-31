package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-remote/internal/auth"
	"codex-remote/internal/codex"
	"codex-remote/internal/events"
	"codex-remote/internal/policy"
)

type fakeBackend struct {
	mu            sync.Mutex
	insideCWD     string
	requests      []codex.PublicRequest
	calls         []string
	authorized    []string
	malformedList bool
	duplicateList bool
}

func (f *fakeBackend) Call(_ context.Context, method string, params json.RawMessage) (json.RawMessage, *codex.RPCError, error) {
	f.mu.Lock()
	f.calls = append(f.calls, method)
	f.mu.Unlock()
	switch method {
	case "thread/list":
		if f.malformedList {
			return json.RawMessage(`{"data":{"cwd":"/secret"},"leak":"/secret"}`), nil, nil
		}
		data := []any{
			map[string]any{"id": "inside", "cwd": f.insideCWD},
			map[string]any{"id": "outside", "cwd": "/"},
		}
		if f.duplicateList {
			data = append([]any{map[string]any{"id": "inside", "cwd": f.insideCWD}}, data...)
		}
		result, err := json.Marshal(map[string]any{
			"data":       data,
			"nextCursor": nil, "backwardsCursor": nil,
		})
		return result, nil, err
	case "thread/read":
		var input struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(params, &input)
		cwd := f.insideCWD
		if input.ThreadID == "outside" {
			cwd = "/"
		}
		result, err := json.Marshal(map[string]any{"thread": map[string]any{"id": input.ThreadID, "cwd": cwd, "turns": []any{}}})
		return result, nil, err
	case "model/list":
		return json.RawMessage(`{"data":[],"nextCursor":null}`), nil, nil
	default:
		return json.RawMessage(`{}`), nil, nil
	}
}

func (f *fakeBackend) PendingRequests() []codex.PublicRequest { return f.requests }
func (f *fakeBackend) Respond(context.Context, string, json.RawMessage) error {
	return nil
}
func (f *fakeBackend) Status() codex.Status { return codex.Status{} }
func (f *fakeBackend) AuthorizeThread(threadID string) {
	f.mu.Lock()
	f.authorized = append(f.authorized, threadID)
	f.mu.Unlock()
}

func newTestServer(t *testing.T) (http.Handler, *fakeBackend, string) {
	return newTestServerWithTrustedProxy(t, false)
}

func newTestServerWithTrustedProxy(t *testing.T, trustedProxy bool) (http.Handler, *fakeBackend, string) {
	webServer, backend, password := newTestServerInstance(t, trustedProxy)
	return webServer.Handler(), backend, password
}

func newTestServerInstance(t *testing.T, trustedProxy bool) (*Server, *fakeBackend, string) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	web := filepath.Join(root, "web")
	if err := os.Mkdir(web, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "index.html"), []byte("<!doctype html><title>test</title>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(web, "styles.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	paths, err := policy.NewPaths([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	backend := &fakeBackend{insideCWD: project}
	sessionKey := "test-session-signing-key-abcdefghijklmnopqrstuvwxyz"
	password := "correct horse battery staple"
	webServer, err := New(Config{
		Password: password, SessionKey: sessionKey, WebRoot: web,
		SessionTTL: time.Hour, TrustedProxy: trustedProxy, Version: "test", Paths: paths,
	}, backend, events.New(8, 4096))
	if err != nil {
		t.Fatal(err)
	}
	return webServer, backend, password
}

func loginCookie(t *testing.T, handler http.Handler, password string) *http.Cookie {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://receiver.test/api/login", bytes.NewBufferString(`{"password":"`+password+`"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", recorder.Code, recorder.Body.String())
	}
	response := recorder.Result()
	cookies := response.Cookies()
	_ = response.Body.Close()
	if len(cookies) != 1 {
		t.Fatalf("expected one login cookie, got %d", len(cookies))
	}
	return cookies[0]
}

func doRequest(handler http.Handler, method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "http://receiver.test"+path, bytes.NewBufferString(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestDirectoryBrowserListsOnlyDirectoriesInsideWorkspace(t *testing.T) {
	handler, backend, password := newTestServer(t)
	root := filepath.Dir(backend.insideCWD)
	if err := os.Mkdir(filepath.Join(root, "another-project"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "not-a-directory"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cookie := loginCookie(t, handler, password)
	recorder := doRequest(handler, http.MethodGet, "/api/directories?path="+url.QueryEscape(root), "", cookie, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("directory response = %d %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Path    string `json:"path"`
		Parent  string `json:"parent"`
		Entries []struct {
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Path != root || payload.Parent != "/" {
		t.Fatalf("directory location = %q parent %q", payload.Path, payload.Parent)
	}
	names := make([]string, 0, len(payload.Entries))
	for _, entry := range payload.Entries {
		names = append(names, entry.Name)
	}
	if strings.Contains(strings.Join(names, ","), "not-a-directory") || !strings.Contains(strings.Join(names, ","), "another-project") {
		t.Fatalf("unexpected directory entries: %#v", names)
	}
}

func TestAttachThreadRuntimeReadsLatestAppliedSettings(t *testing.T) {
	threadID := "thread-123"
	rolloutPath := filepath.Join(t.TempDir(), "rollout-"+threadID+".jsonl")
	lines := strings.Join([]string{
		`{"type":"turn_context","payload":{"model":"gpt-old","effort":"low","service_tier":null,"personality":"friendly"}}`,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"model":"gpt-5.6-sol","reasoning_effort":"max","service_tier":"priority","personality":"pragmatic"}}}`,
		`{"type":"response_item","payload":{"type":"message"}}`,
	}, "\n")
	if err := os.WriteFile(rolloutPath, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": threadID, "cwd": "/tmp", "path": rolloutPath}})
	var envelope struct {
		Thread struct {
			Runtime struct {
				Model       string `json:"model"`
				Effort      string `json:"effort"`
				ServiceTier string `json:"serviceTier"`
				Personality string `json:"personality"`
			} `json:"runtime"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(attachThreadRuntime(raw), &envelope); err != nil {
		t.Fatal(err)
	}
	if got := envelope.Thread.Runtime; got.Model != "gpt-5.6-sol" || got.Effort != "max" || got.ServiceTier != "priority" || got.Personality != "pragmatic" {
		t.Fatalf("runtime = %#v", got)
	}
}

func proxiedLogin(handler http.Handler, forwardedFor string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "http://receiver.test/api/login", strings.NewReader(`{"password":"definitely-not-the-right-password"}`))
	request.RemoteAddr = "127.0.0.1:41234"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Forwarded-For", forwardedFor)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestClientIPHonorsTrustedProxyBoundary(t *testing.T) {
	tests := []struct {
		name         string
		trustedProxy bool
		remoteAddr   string
		forwardedFor []string
		want         string
	}{
		{
			name: "loopback proxy is trusted automatically", remoteAddr: "127.0.0.1:41234",
			forwardedFor: []string{"203.0.113.8"}, want: "203.0.113.8",
		},
		{
			name: "explicit trust accepts non-loopback proxy", trustedProxy: true, remoteAddr: "192.0.2.44:41234",
			forwardedFor: []string{"203.0.113.8"}, want: "203.0.113.8",
		},
		{
			name: "non-loopback peer is ignored without explicit trust", remoteAddr: "192.0.2.44:41234",
			forwardedFor: []string{"203.0.113.8"}, want: "192.0.2.44",
		},
		{
			name: "rightmost address wins", trustedProxy: true, remoteAddr: "127.0.0.1:41234",
			forwardedFor: []string{"198.51.100.19, 203.0.113.8"}, want: "203.0.113.8",
		},
		{
			name: "rightmost duplicate field wins", trustedProxy: true, remoteAddr: "[::1]:41234",
			forwardedFor: []string{"198.51.100.19", "2001:db8::8"}, want: "2001:db8::8",
		},
		{
			name: "invalid rightmost address fails closed", trustedProxy: true, remoteAddr: "127.0.0.1:41234",
			forwardedFor: []string{"203.0.113.8, unknown"}, want: "127.0.0.1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "http://receiver.test/api/login", nil)
			request.RemoteAddr = test.remoteAddr
			for _, value := range test.forwardedFor {
				request.Header.Add("X-Forwarded-For", value)
			}
			server := &Server{cfg: Config{TrustedProxy: test.trustedProxy}}
			if got := server.clientIP(request); got != test.want {
				t.Fatalf("clientIP() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTrustedProxyLoginRateLimitUsesForwardedClient(t *testing.T) {
	t.Run("separate public clients have separate limits", func(t *testing.T) {
		handler, _, _ := newTestServerWithTrustedProxy(t, true)
		for attempt := 0; attempt < maxLoginFailures; attempt++ {
			if got := proxiedLogin(handler, "203.0.113.8").Code; got != http.StatusUnauthorized {
				t.Fatalf("client A attempt %d status = %d", attempt+1, got)
			}
		}
		if got := proxiedLogin(handler, "203.0.113.8").Code; got != http.StatusTooManyRequests {
			t.Fatalf("client A over-limit status = %d", got)
		}
		if got := proxiedLogin(handler, "203.0.113.9").Code; got != http.StatusUnauthorized {
			t.Fatalf("client B inherited client A limit: status = %d", got)
		}
	})

	t.Run("forged left side cannot rotate the limit key", func(t *testing.T) {
		handler, _, _ := newTestServerWithTrustedProxy(t, true)
		for attempt := 0; attempt < maxLoginFailures; attempt++ {
			forwardedFor := fmt.Sprintf("198.51.100.%d, 203.0.113.8", attempt+1)
			if got := proxiedLogin(handler, forwardedFor).Code; got != http.StatusUnauthorized {
				t.Fatalf("attempt %d status = %d", attempt+1, got)
			}
		}
		if got := proxiedLogin(handler, "198.51.100.99, 203.0.113.8").Code; got != http.StatusTooManyRequests {
			t.Fatalf("forged chain bypassed rate limit: status = %d", got)
		}
	})

	t.Run("malformed requests do not consume password attempts", func(t *testing.T) {
		handler, _, _ := newTestServerWithTrustedProxy(t, true)
		for attempt := 0; attempt < maxLoginFailures+5; attempt++ {
			request := httptest.NewRequest(http.MethodPost, "http://receiver.test/api/login", strings.NewReader("{"))
			request.RemoteAddr = "127.0.0.1:41234"
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Forwarded-For", "203.0.113.10")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("malformed attempt %d status = %d", attempt+1, recorder.Code)
			}
		}
		if got := proxiedLogin(handler, "203.0.113.10").Code; got != http.StatusUnauthorized {
			t.Fatalf("malformed requests consumed the password limit: status = %d", got)
		}
	})
}

func TestAuthenticationAndMethodAllowlist(t *testing.T) {
	handler, _, token := newTestServer(t)
	recorder := doRequest(handler, http.MethodGet, "/api/status", "", nil, "")
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", recorder.Code)
	}
	cookie := loginCookie(t, handler, token)
	recorder = doRequest(handler, http.MethodPost, "/api/rpc", `{"method":"config/batchWrite","params":{}}`, cookie, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("forbidden method status = %d", recorder.Code)
	}
}

func TestThreadListFiltersOutsideWorkspaces(t *testing.T) {
	handler, backend, token := newTestServer(t)
	backend.duplicateList = true
	cookie := loginCookie(t, handler, token)
	recorder := doRequest(handler, http.MethodPost, "/api/rpc", `{"method":"thread/list","params":{"limit":20}}`, cookie, "")
	var body struct {
		Result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Result.Data) != 1 || body.Result.Data[0].ID != "inside" {
		t.Fatalf("unexpected filtered threads: %#v", body.Result.Data)
	}
	backend.mu.Lock()
	authorized := append([]string(nil), backend.authorized...)
	backend.mu.Unlock()
	if len(authorized) != 1 || authorized[0] != "inside" {
		t.Fatalf("filtered thread ids were not authorized: %#v", authorized)
	}
}

func TestRichMethodsUseStrictPolicyAndThreadAuthorization(t *testing.T) {
	handler, backend, token := newTestServer(t)
	cookie := loginCookie(t, handler, token)

	for _, body := range []string{
		`{"method":"thread/compact/start","params":{"threadId":"inside"}}`,
		`{"method":"review/start","params":{"threadId":"inside","target":{"type":"uncommittedChanges"}}}`,
	} {
		recorder := doRequest(handler, http.MethodPost, "/api/rpc", body, cookie, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("rich method failed: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	}
	backend.mu.Lock()
	calls := append([]string(nil), backend.calls...)
	backend.mu.Unlock()
	if !containsCall(calls, "thread/compact/start") || !containsCall(calls, "review/start") {
		t.Fatalf("rich methods did not reach backend: %#v", calls)
	}

	recorder := doRequest(handler, http.MethodPost, "/api/rpc", `{"method":"review/start","params":{"threadId":"inside","target":{"type":"uncommittedChanges"},"delivery":"detached"}}`, cookie, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("detached review escaped policy: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = doRequest(handler, http.MethodPost, "/api/rpc", `{"method":"modelProvider/capabilities/read","params":{"provider":"override"}}`, cookie, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("provider override escaped policy: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func containsCall(calls []string, wanted string) bool {
	for _, call := range calls {
		if call == wanted {
			return true
		}
	}
	return false
}

func TestMalformedThreadListFailsClosed(t *testing.T) {
	handler, backend, token := newTestServer(t)
	backend.malformedList = true
	cookie := loginCookie(t, handler, token)
	recorder := doRequest(handler, http.MethodPost, "/api/rpc", `{"method":"thread/list","params":{"limit":20}}`, cookie, "")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("malformed thread list status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if bytes.Contains(recorder.Body.Bytes(), []byte("/secret")) {
		t.Fatalf("malformed backend response leaked through: %s", recorder.Body.String())
	}
}

func TestOriginAndSecurityHeaders(t *testing.T) {
	handler, _, token := newTestServer(t)
	cookie := loginCookie(t, handler, token)
	recorder := doRequest(handler, http.MethodPost, "/api/rpc", `{"method":"model/list","params":{}}`, cookie, "https://evil.example")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin request status = %d", recorder.Code)
	}
	if recorder.Header().Get("Content-Security-Policy") == "" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("security headers are missing")
	}
}

func TestStaticShellAssetsAlwaysRevalidate(t *testing.T) {
	handler, _, _ := newTestServer(t)
	for target, wantStatus := range map[string]int{"/": http.StatusOK, "/index.html": http.StatusMovedPermanently, "/styles.css?v=20260830.8": http.StatusOK} {
		recorder := doRequest(handler, http.MethodGet, target, "", nil, "")
		if recorder.Code != wantStatus {
			t.Fatalf("static %s status = %d, want %d; body=%s", target, recorder.Code, wantStatus, recorder.Body.String())
		}
		if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
			t.Fatalf("static %s Cache-Control = %q, want no-cache", target, got)
		}
	}
}

func TestReverseProxyOriginMatrix(t *testing.T) {
	tests := []struct {
		name           string
		trustedProxy   bool
		remoteAddr     string
		host           string
		origin         string
		fetchSite      string
		forwardedProto string
		forwardedHost  string
		forwardedPort  string
		wantStatus     int
		wantCookie     string
		wantSecure     bool
	}{
		{
			name: "loopback proxy chain with rewritten host", remoteAddr: "127.0.0.1:41234",
			host: "127.0.0.1:18787", origin: "https://remote.example", fetchSite: "same-origin",
			forwardedProto: "https, http", forwardedHost: "remote.example, receiver.example",
			forwardedPort: "443, 80", wantStatus: http.StatusOK,
			wantCookie: "__Host-codex_remote_session", wantSecure: true,
		},
		{
			name: "canonical https default port", remoteAddr: "[::1]:41234",
			host: "receiver.example:18787", origin: "https://remote.example",
			forwardedProto: "https", forwardedHost: "remote.example:443",
			wantStatus: http.StatusOK, wantCookie: "__Host-codex_remote_session", wantSecure: true,
		},
		{
			name: "forwarded non-default port", remoteAddr: "127.0.0.1:41234",
			host: "receiver.example:18787", origin: "https://remote.example:8443",
			forwardedProto: "https", forwardedHost: "remote.example", forwardedPort: "8443",
			wantStatus: http.StatusOK, wantCookie: "__Host-codex_remote_session", wantSecure: true,
		},
		{
			name: "explicitly trusted non-loopback proxy", trustedProxy: true, remoteAddr: "192.0.2.44:41234",
			host: "receiver.example:18787", origin: "https://remote.example",
			forwardedProto: "https", forwardedHost: "remote.example", forwardedPort: "443",
			wantStatus: http.StatusOK, wantCookie: "__Host-codex_remote_session", wantSecure: true,
		},
		{
			name: "fetch metadata fallback for rewritten host", remoteAddr: "127.0.0.1:41234",
			host: "receiver.example:18787", origin: "https://remote.example", fetchSite: "same-origin",
			forwardedProto: "https", wantStatus: http.StatusOK,
			wantCookie: "__Host-codex_remote_session", wantSecure: true,
		},
		{
			name: "direct http canonical default port", remoteAddr: "192.0.2.44:41234",
			host: "receiver.test", origin: "http://receiver.test:80",
			wantStatus: http.StatusOK, wantCookie: "codex_remote_dev",
		},
		{
			name: "untrusted non-loopback forwarded headers", remoteAddr: "192.0.2.44:41234",
			host: "receiver.example:18787", origin: "https://remote.example",
			forwardedProto: "https", forwardedHost: "remote.example", forwardedPort: "443",
			wantStatus: http.StatusForbidden,
		},
		{
			name: "forwarded port mismatch", remoteAddr: "127.0.0.1:41234",
			host: "receiver.example:18787", origin: "https://remote.example:8443",
			forwardedProto: "https", forwardedHost: "remote.example", forwardedPort: "443",
			wantStatus: http.StatusForbidden,
		},
		{
			name: "mismatched origin without browser fallback", remoteAddr: "127.0.0.1:41234",
			host: "receiver.example:18787", origin: "https://evil.example",
			forwardedProto: "https", forwardedHost: "remote.example",
			wantStatus: http.StatusForbidden,
		},
		{
			name: "explicit cross-site rejects matching authority", remoteAddr: "127.0.0.1:41234",
			host: "receiver.example:18787", origin: "https://remote.example", fetchSite: "cross-site",
			forwardedProto: "https", forwardedHost: "remote.example",
			wantStatus: http.StatusForbidden,
		},
		{
			name: "explicit cross-site rejects absent origin", remoteAddr: "127.0.0.1:41234",
			host: "receiver.example:18787", fetchSite: "cross-site",
			forwardedProto: "https", forwardedHost: "remote.example",
			wantStatus: http.StatusForbidden,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, _, password := newTestServerWithTrustedProxy(t, test.trustedProxy)
			request := httptest.NewRequest(http.MethodPost, "http://receiver.example/api/login", bytes.NewBufferString(`{"password":"`+password+`"}`))
			request.RemoteAddr = test.remoteAddr
			request.Host = test.host
			request.Header.Set("Content-Type", "application/json")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			if test.fetchSite != "" {
				request.Header.Set("Sec-Fetch-Site", test.fetchSite)
			}
			if test.forwardedProto != "" {
				request.Header.Set("X-Forwarded-Proto", test.forwardedProto)
			}
			if test.forwardedHost != "" {
				request.Header.Set("X-Forwarded-Host", test.forwardedHost)
			}
			if test.forwardedPort != "" {
				request.Header.Set("X-Forwarded-Port", test.forwardedPort)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			cookies := recorder.Result().Cookies()
			if test.wantCookie == "" {
				if len(cookies) != 0 {
					t.Fatalf("unexpected cookies: %#v", cookies)
				}
				return
			}
			if len(cookies) != 1 || cookies[0].Name != test.wantCookie || cookies[0].Secure != test.wantSecure {
				t.Fatalf("unexpected login cookie: %#v", cookies)
			}
			if test.wantSecure && recorder.Header().Get("Strict-Transport-Security") == "" {
				t.Fatal("proxied HTTPS response omitted HSTS")
			}
		})
	}
}

func TestTrustedProxySessionCookieNameFallback(t *testing.T) {
	handler, _, token := newTestServer(t)
	t.Run("development cookie on externally secure request", func(t *testing.T) {
		developmentCookie := loginCookie(t, handler, token)
		request := httptest.NewRequest(http.MethodGet, "http://receiver.example/api/status", nil)
		request.RemoteAddr = "127.0.0.1:41234"
		request.Header.Set("X-Forwarded-Proto", "https")
		request.Header.Set("X-Forwarded-Host", "remote.example")
		request.AddCookie(developmentCookie)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("trusted proxy did not accept the development cookie fallback: %d %s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("secure cookie on externally http request", func(t *testing.T) {
		login := httptest.NewRequest(http.MethodPost, "https://receiver.test/api/login", bytes.NewBufferString(`{"password":"`+token+`"}`))
		login.Header.Set("Content-Type", "application/json")
		login.Header.Set("Origin", "https://receiver.test")
		loginResponse := httptest.NewRecorder()
		handler.ServeHTTP(loginResponse, login)
		cookies := loginResponse.Result().Cookies()
		if loginResponse.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Name != "__Host-codex_remote_session" {
			t.Fatalf("secure login failed: status=%d cookies=%#v", loginResponse.Code, cookies)
		}

		request := httptest.NewRequest(http.MethodGet, "http://receiver.example/api/status", nil)
		request.RemoteAddr = "127.0.0.1:41234"
		request.AddCookie(cookies[0])
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("trusted proxy did not accept the secure cookie fallback: %d %s", recorder.Code, recorder.Body.String())
		}
	})
}

func TestLoopbackTLSProxySessionRoundTrip(t *testing.T) {
	webServer, backend, token := newTestServerInstance(t, false)
	handler := webServer.Handler()
	setProxyHeaders := func(request *http.Request) {
		request.RemoteAddr = "127.0.0.1:41234"
		request.Host = "127.0.0.1:18787"
		request.Header.Set("X-Forwarded-Proto", "https")
		request.Header.Set("X-Forwarded-Host", "remote.example")
		request.Header.Set("X-Forwarded-Port", "443")
	}

	login := httptest.NewRequest(http.MethodPost, "http://receiver.example/api/login", bytes.NewBufferString(`{"password":"`+token+`"}`))
	setProxyHeaders(login)
	login.Header.Set("Content-Type", "application/json")
	login.Header.Set("Origin", "https://remote.example")
	login.Header.Set("Sec-Fetch-Site", "same-origin")
	loginResponse := httptest.NewRecorder()
	handler.ServeHTTP(loginResponse, login)
	cookies := loginResponse.Result().Cookies()
	if loginResponse.Code != http.StatusOK || len(cookies) != 1 || cookies[0].Name != "__Host-codex_remote_session" {
		t.Fatalf("proxied login failed: status=%d cookies=%#v body=%s", loginResponse.Code, cookies, loginResponse.Body.String())
	}
	cookie := cookies[0]

	status := httptest.NewRequest(http.MethodGet, "http://receiver.example/api/status", nil)
	setProxyHeaders(status)
	status.AddCookie(cookie)
	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, status)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("proxied session check failed: %d %s", statusResponse.Code, statusResponse.Body.String())
	}

	rpc := httptest.NewRequest(http.MethodPost, "http://receiver.example/api/rpc", strings.NewReader(`{"method":"model/list","params":{}}`))
	setProxyHeaders(rpc)
	rpc.Header.Set("Content-Type", "application/json")
	rpc.Header.Set("Origin", "https://remote.example")
	rpc.Header.Set("Sec-Fetch-Site", "same-origin")
	rpc.AddCookie(cookie)
	rpcResponse := httptest.NewRecorder()
	handler.ServeHTTP(rpcResponse, rpc)
	if rpcResponse.Code != http.StatusOK {
		t.Fatalf("proxied RPC failed: %d %s", rpcResponse.Code, rpcResponse.Body.String())
	}
	backend.mu.Lock()
	calls := append([]string(nil), backend.calls...)
	backend.mu.Unlock()
	if !containsCall(calls, "model/list") {
		t.Fatalf("proxied RPC did not reach the backend: %#v", calls)
	}

	events := httptest.NewRequest(http.MethodGet, "http://receiver.example/api/events", nil)
	setProxyHeaders(events)
	events.Header.Set("Origin", "https://remote.example")
	events.Header.Set("Sec-Fetch-Site", "same-origin")
	events.AddCookie(cookie)
	streamResponse := newStreamRecorder()
	streamDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(streamResponse, events)
		close(streamDone)
	}()
	select {
	case <-streamResponse.flushed:
	case <-streamDone:
		t.Fatal("proxied event stream closed before its greeting")
	case <-time.After(time.Second):
		t.Fatal("proxied event stream did not flush its greeting")
	}

	logout := httptest.NewRequest(http.MethodPost, "http://receiver.example/api/logout", strings.NewReader(`{}`))
	setProxyHeaders(logout)
	logout.Header.Set("Content-Type", "application/json")
	logout.Header.Set("Origin", "https://remote.example")
	logout.Header.Set("Sec-Fetch-Site", "same-origin")
	logout.AddCookie(cookie)
	logoutResponse := httptest.NewRecorder()
	handler.ServeHTTP(logoutResponse, logout)
	if logoutResponse.Code != http.StatusOK {
		t.Fatalf("proxied logout failed: %d %s", logoutResponse.Code, logoutResponse.Body.String())
	}
	waitForStreamClose(t, streamDone)
}

func TestArtifactRouteServesOnlyAllowedRasterImages(t *testing.T) {
	handler, backend, token := newTestServer(t)
	cookie := loginCookie(t, handler, token)
	imagePath := filepath.Join(backend.insideCWD, "shot.png")
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	if err := os.WriteFile(imagePath, png, 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := doRequest(handler, http.MethodGet, "/api/artifact?path="+url.QueryEscape(imagePath), "", cookie, "")
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "image/png" || !bytes.Equal(recorder.Body.Bytes(), png) {
		t.Fatalf("allowed artifact response: status=%d type=%q body=%q", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.Bytes())
	}

	textPath := filepath.Join(backend.insideCWD, "secret.txt")
	if err := os.WriteFile(textPath, []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder = doRequest(handler, http.MethodGet, "/api/artifact?path="+url.QueryEscape(textPath), "", cookie, "")
	if recorder.Code != http.StatusUnsupportedMediaType || bytes.Contains(recorder.Body.Bytes(), []byte("not an image")) {
		t.Fatalf("non-image artifact leaked: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = doRequest(handler, http.MethodGet, "/api/artifact?path="+url.QueryEscape("/etc/passwd"), "", cookie, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("out-of-root artifact status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestProjectFilesAreScopedToThreadWorkspace(t *testing.T) {
	webServer, backend, token := newTestServerInstance(t, false)
	handler := webServer.Handler()
	cookie := loginCookie(t, handler, token)
	sourceDir := filepath.Join(backend.insideCWD, "src")
	if err := os.Mkdir(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceDir, "main.go")
	if err := os.WriteFile(sourcePath, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binaryPath := filepath.Join(sourceDir, "data.bin")
	if err := os.WriteFile(binaryPath, []byte{'a', 0, 'b'}, 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(backend.insideCWD, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"password":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	escape := filepath.Join(backend.insideCWD, "escape")
	if err := os.Symlink(t.TempDir(), escape); err != nil {
		t.Fatal(err)
	}

	root := doRequest(handler, http.MethodGet, "/api/files?threadId=inside", "", cookie, "")
	if root.Code != http.StatusOK || !bytes.Contains(root.Body.Bytes(), []byte(`"name":"src"`)) || !bytes.Contains(root.Body.Bytes(), []byte(`"name":"config.json"`)) || bytes.Contains(root.Body.Bytes(), []byte(`"name":"escape"`)) {
		t.Fatalf("project root hid project files: status=%d body=%s", root.Code, root.Body.String())
	}

	query := "/api/files?threadId=inside&path=" + url.QueryEscape("src")
	recorder := doRequest(handler, http.MethodGet, query, "", cookie, "")
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte(`"path":"src"`)) || !bytes.Contains(recorder.Body.Bytes(), []byte(`"name":"main.go"`)) {
		t.Fatalf("project directory response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	recorder = doRequest(handler, http.MethodGet, "/api/files?threadId=inside&path=src%2Fmain.go", "", cookie, "")
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "text/plain; charset=utf-8" || recorder.Body.String() != "package main\n" {
		t.Fatalf("project preview response: status=%d type=%q body=%q", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	recorder = doRequest(handler, http.MethodGet, "/api/files?threadId=inside&path=src%2Fdata.bin", "", cookie, "")
	if recorder.Code != http.StatusUnsupportedMediaType || bytes.Contains(recorder.Body.Bytes(), []byte{'a', 0, 'b'}) {
		t.Fatalf("binary preview response: status=%d body=%q", recorder.Code, recorder.Body.Bytes())
	}
	recorder = doRequest(handler, http.MethodGet, "/api/files?threadId=inside&path=src%2Fdata.bin&download=1", "", cookie, "")
	if recorder.Code != http.StatusOK || !strings.HasPrefix(recorder.Header().Get("Content-Disposition"), "attachment;") || !bytes.Equal(recorder.Body.Bytes(), []byte{'a', 0, 'b'}) {
		t.Fatalf("project download response: status=%d disposition=%q body=%q", recorder.Code, recorder.Header().Get("Content-Disposition"), recorder.Body.Bytes())
	}
	recorder = doRequest(handler, http.MethodGet, "/api/files?threadId=inside&path=..%2Fanother-project", "", cookie, "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("project traversal status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	recorder = doRequest(handler, http.MethodGet, "/api/files?threadId=inside&path=config.json&download=1", "", cookie, "")
	if recorder.Code != http.StatusOK || !bytes.Contains(recorder.Body.Bytes(), []byte("secret")) {
		t.Fatalf("project file was not downloadable: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestUploadStoresSmallAuthenticatedFile(t *testing.T) {
	handler, _, token := newTestServer(t)
	cookie := loginCookie(t, handler, token)
	data := []byte("remote attachment")
	body, _ := json.Marshal(map[string]any{
		"name": "notes.txt",
		"data": "data:text/plain;base64," + base64.StdEncoding.EncodeToString(data),
	})
	recorder := doRequest(handler, http.MethodPost, "/api/uploads", string(body), cookie, "")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload response: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Size int    `json:"size"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(result.Path) })
	stored, err := os.ReadFile(result.Path)
	if err != nil || result.Name != "notes.txt" || result.Size != len(data) || !bytes.Equal(stored, data) {
		t.Fatalf("stored upload: result=%#v data=%q err=%v", result, stored, err)
	}
}

func TestArtifactOpenRejectsSymlinkSwapAfterValidation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("secure artifact opening uses Linux openat2")
	}
	root := t.TempDir()
	paths, err := policy.NewPaths([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	liveDirectory := filepath.Join(root, "live")
	if err := os.Mkdir(liveDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	requested := filepath.Join(liveDirectory, "shot.png")
	if err := os.WriteFile(requested, []byte("allowed"), 0o644); err != nil {
		t.Fatal(err)
	}
	canonical, err := paths.CheckTarget(requested)
	if err != nil {
		t.Fatal(err)
	}

	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "shot.png")
	if err := os.WriteFile(outsideFile, []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(liveDirectory, filepath.Join(root, "validated-live")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, liveDirectory); err != nil {
		t.Fatal(err)
	}

	// This is the exact old check-then-os.Open primitive: after the directory
	// swap it resolves to the out-of-root inode, proving the regression setup.
	unsafeFile, err := os.Open(canonical)
	if err != nil {
		t.Fatal(err)
	}
	unsafeInfo, err := unsafeFile.Stat()
	_ = unsafeFile.Close()
	if err != nil {
		t.Fatal(err)
	}
	outsideInfo, err := os.Stat(outsideFile)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(unsafeInfo, outsideInfo) {
		t.Fatal("test setup did not redirect the old path open outside the allowed root")
	}

	file, err := openArtifactFile(canonical)
	if err == nil {
		openedInfo, statErr := file.Stat()
		_ = file.Close()
		if statErr == nil && os.SameFile(openedInfo, outsideInfo) {
			t.Fatal("secure artifact open returned an out-of-root file descriptor")
		}
		t.Fatal("secure artifact open accepted a symlink swapped after validation")
	}
	if !errors.Is(err, errArtifactPathChanged) {
		t.Fatalf("symlink swap error = %v, want errArtifactPathChanged", err)
	}
}

func TestArtifactOpenPinsTheValidatedFileDescriptor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("secure artifact opening uses Linux openat2")
	}
	root := t.TempDir()
	allowedPath := filepath.Join(root, "shot.png")
	allowedBytes := []byte("allowed-image-object")
	if err := os.WriteFile(allowedPath, allowedBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := openArtifactFile(allowedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	outside := t.TempDir()
	outsidePath := filepath.Join(outside, "secret.png")
	if err := os.WriteFile(outsidePath, []byte("outside-secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	pinnedPath := allowedPath + ".pinned"
	if err := os.Rename(allowedPath, pinnedPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePath, allowedPath); err != nil {
		t.Fatal(err)
	}

	fdInfo, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	pinnedInfo, err := os.Stat(pinnedPath)
	if err != nil {
		t.Fatal(err)
	}
	outsideInfo, err := os.Stat(outsidePath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(fdInfo, pinnedInfo) || os.SameFile(fdInfo, outsideInfo) {
		t.Fatal("opened descriptor was not pinned to the validated in-root inode")
	}
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, allowedBytes) {
		t.Fatalf("pinned descriptor read %q, want %q", got, allowedBytes)
	}
}

func TestSessionCookieInvalidAfterSessionKeyChange(t *testing.T) {
	sessionKey := "abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	cookie, err := auth.IssueSession(sessionKey, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if auth.ValidateSession(sessionKey+"x", cookie, time.Now()) {
		t.Fatal("session survived a signing-key change")
	}
}

type streamRecorder struct {
	header  http.Header
	flushed chan struct{}
	once    sync.Once
}

func newStreamRecorder() *streamRecorder {
	return &streamRecorder{header: make(http.Header), flushed: make(chan struct{})}
}

func (r *streamRecorder) Header() http.Header { return r.header }
func (r *streamRecorder) Write(data []byte) (int, error) {
	return len(data), nil
}
func (r *streamRecorder) WriteHeader(int) {}
func (r *streamRecorder) Flush() {
	r.once.Do(func() { close(r.flushed) })
}

func openEventStream(t *testing.T, handler http.Handler, cookie *http.Cookie) <-chan struct{} {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "http://receiver.test/api/events", nil)
	request.AddCookie(cookie)
	recorder := newStreamRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(recorder, request)
		close(done)
	}()
	select {
	case <-recorder.flushed:
	case <-done:
		t.Fatal("event stream closed before its greeting")
	case <-time.After(time.Second):
		t.Fatal("event stream did not flush its greeting")
	}
	return done
}

func waitForStreamClose(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("event stream remained open after session revocation")
	}
}

func TestLogoutRevokesExistingEventStream(t *testing.T) {
	webServer, _, password := newTestServerInstance(t, false)
	cookie := loginCookie(t, webServer.Handler(), password)
	streamDone := openEventStream(t, webServer.Handler(), cookie)

	logout := httptest.NewRequest(http.MethodPost, "http://receiver.test/api/logout", nil)
	logout.AddCookie(cookie)
	recorder := httptest.NewRecorder()
	webServer.Handler().ServeHTTP(recorder, logout)
	if recorder.Code != http.StatusOK {
		t.Fatalf("logout status = %d", recorder.Code)
	}
	waitForStreamClose(t, streamDone)

	status := doRequest(webServer.Handler(), http.MethodGet, "/api/status", "", cookie, "")
	if status.Code != http.StatusUnauthorized {
		t.Fatalf("logged-out cookie remained valid: %d", status.Code)
	}
}

func TestHTTPSLoginUsesHostPrefixedCookie(t *testing.T) {
	handler, _, password := newTestServer(t)
	request := httptest.NewRequest(http.MethodPost, "https://receiver.test/api/login", bytes.NewBufferString(`{"password":"`+password+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://receiver.test")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	response := recorder.Result()
	defer response.Body.Close()
	cookies := response.Cookies()
	if recorder.Code != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("secure login failed: status=%d cookies=%d", recorder.Code, len(cookies))
	}
	if cookies[0].Name != "__Host-codex_remote_session" || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("unexpected secure cookie: %#v", cookies[0])
	}
}

func TestHTTPSDoesNotAcceptDevelopmentCookie(t *testing.T) {
	handler, _, token := newTestServer(t)
	developmentCookie := loginCookie(t, handler, token)
	request := httptest.NewRequest(http.MethodGet, "https://receiver.test/api/status", nil)
	request.AddCookie(developmentCookie)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("development cookie authenticated HTTPS request: %d", recorder.Code)
	}
}

func TestDecodeJSONRejectsOversizedTrailingWhitespace(t *testing.T) {
	body := `{"password":"ok"}` + strings.Repeat(" ", maxRequestBody)
	request := httptest.NewRequest(http.MethodPost, "http://receiver.test/api/login", strings.NewReader(body))
	var target struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(request, &target); err == nil {
		t.Fatal("oversized JSON body was accepted")
	}
}

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/pelletier/go-toml/v2"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-remote/internal/codex"
	"codex-remote/internal/policy"
)

func TestUsersIsolationAndRevocation(t *testing.T) {
	s, backend, password := newTestServerInstance(t, false)
	s.cfg.CodexHome = filepath.Join(filepath.Dir(backend.insideCWD), ".codex")
	factory := func(u User, home, uploads string, p *policy.Paths, emit func(any)) (Backend, error) {
		return &fakeBackend{insideCWD: u.Workspaces[0]}, nil
	}
	if err := s.EnableUsers(factory); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	admin := loginCookie(t, h, password)
	save := func(user User, password string) *httptest.ResponseRecorder {
		data, _ := json.Marshal(map[string]any{"action": "save", "user": user, "password": password})
		return doRequest(h, "POST", "/api/users", string(data), admin, "")
	}
	u := User{Name: "Alice", Workspaces: []string{backend.insideCWD}, Models: []string{"allowed-model"}, Efforts: []string{"low"}, ServiceTiers: []string{"default"}, Permissions: []string{"chat", "files", "uploads"}, Access: "read-only"}
	created := save(u, "alice-password-123")
	if created.Code != 200 {
		t.Fatal(created.Body.String())
	}
	var response struct{ User User }
	_ = json.Unmarshal(created.Body.Bytes(), &response)
	u = response.User
	if u.ID == "" || strings.Contains(created.Body.String(), "passwordHash") {
		t.Fatal("invalid public user")
	}
	alice := loginCookie(t, h, "alice-password-123")
	for _, path := range []string{"/api/users", "/api/codex/settings", "/api/requests"} {
		if got := doRequest(h, "GET", path, "", alice, ""); got.Code != 403 {
			t.Fatalf("%s leaked: %d %s", path, got.Code, got.Body.String())
		}
	}
	for _, path := range []string{"/api/directories?path=/root", "/api/files?path=/etc/passwd", "/api/artifact?path=/etc/passwd"} {
		if got := doRequest(h, "GET", path, "", alice, ""); got.Code == 200 {
			t.Fatalf("%s leaked", path)
		}
	}
	if err := os.Symlink("/etc", filepath.Join(backend.insideCWD, "escape")); err != nil {
		t.Fatal(err)
	}
	if got := doRequest(h, "GET", "/api/files?path="+backend.insideCWD+"/escape/passwd", "", alice, ""); got.Code == 200 {
		t.Fatal("symlink escaped")
	}
	for _, params := range []string{`{"cwd":"/","model":"allowed-model"}`, `{"cwd":` + quote(backend.insideCWD) + `,"model":"forbidden"}`, `{"cwd":` + quote(backend.insideCWD) + `,"serviceTier":"fast"}`} {
		got := doRequest(h, "POST", "/api/rpc", `{"method":"thread/start","params":`+params+`}`, alice, "")
		if got.Code != 403 {
			t.Fatalf("launch escaped: %s", got.Body.String())
		}
	}
	if got := doRequest(h, "POST", "/api/rpc", `{"method":"thread/read","params":{"threadId":"outside"}}`, alice, ""); got.Code != 403 {
		t.Fatal("outside thread allowed")
	}
	upload := doRequest(h, "POST", "/api/uploads?name=alice.txt", "private text", alice, "")
	if upload.Code != 201 && upload.Code != 200 {
		t.Fatal(upload.Body.String())
	}
	bob := u
	bob.ID = ""
	bob.Revision = ""
	bob.Name = "Bob"
	r := save(bob, "bob-password-123")
	if r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	bobCookie := loginCookie(t, h, "bob-password-123")
	if got := doRequest(h, "GET", "/api/uploads", "", bobCookie, ""); strings.Contains(got.Body.String(), "alice.txt") {
		t.Fatal("cross-user upload leak")
	}
	aliceServer, _ := s.users.server(u.ID, false)
	raw, err := aliceServer.sanitizeUserParams("turn/start", json.RawMessage(`{"threadId":"inside","input":[{"type":"text","text":"x"}],"sandboxPolicy":{"type":"dangerFullAccess"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var params map[string]any
	_ = json.Unmarshal(raw, &params)
	if params["model"] != "allowed-model" || params["effort"] != "low" || params["permissions"] != "remote-"+u.ID+"-"+u.Revision {
		t.Fatal(string(raw))
	}
	if save(User{Name: "bad", Workspaces: []string{"/"}, Access: "read-only"}, "other-password-123").Code != 400 {
		t.Fatal("root workspace allowed")
	}
	duplicate := bob
	duplicate.ID = ""
	if save(duplicate, "alice-password-123").Code != 400 {
		t.Fatal("duplicate password allowed")
	}
	u.Disabled = true
	if r := save(u, ""); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	if got := doRequest(h, "GET", "/api/status", "", alice, ""); got.Code != 401 {
		t.Fatal("disabled user session survived")
	}
	// The registry survives restart and does not contain plaintext passwords.
	data, _ := os.ReadFile(filepath.Join(s.cfg.UserRoot, "users.json"))
	if strings.Contains(string(data), "alice-password-123") {
		t.Fatal("plaintext password persisted")
	}
	if err := s.EnableUsers(factory); err != nil {
		t.Fatal(err)
	}
	if len(s.users.users) != 2 || !s.users.users[0].Disabled {
		t.Fatal("registry did not survive restart")
	}
	if doRequest(s.Handler(), "GET", "/api/users", "", admin, "").Code != 200 {
		t.Fatal("admin lost access")
	}
}
func quote(v string) string { data, _ := json.Marshal(v); return string(data) }

// Runs the real app-server handshake without a model turn or external API call.
func TestIsolatedUserBackendHandshake(t *testing.T) {
	if os.Getenv("CODEX_REMOTE_TEST_ISOLATION") != "1" {
		t.Skip("set CODEX_REMOTE_TEST_ISOLATION=1 for the installed Linux runtime")
	}
	s, b, _ := newTestServerInstance(t, false)
	s.cfg.CodexHome = filepath.Join(filepath.Dir(b.insideCWD), ".codex")
	marker := filepath.Join(b.insideCWD, "mcp-started")
	base, _ := toml.Marshal(map[string]any{"service_tier": "fast", "mcp_servers": map[string]any{"fixture.with.dot": map[string]any{"command": "/bin/sh", "args": []string{"-c", "touch " + marker}}}})
	if err := os.WriteFile(filepath.Join(s.cfg.CodexHome, "config.toml"), base, 0600); err != nil {
		t.Fatal(err)
	}

	backend, err := codex.New(codex.Config{Mode: "spawn", CodexBin: "auto", CodexHome: s.cfg.CodexHome, IdleTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	s.backend = backend
	if err := s.EnableUsers(s.UserFactory()); err != nil {
		t.Fatal(err)
	}
	u := User{ID: "runtime-test", Revision: "1", Name: "runtime", Workspaces: []string{b.insideCWD}, Access: "read-only", Permissions: []string{"chat"}, ServiceTiers: []string{"default"}}
	s.users.users = []User{u}
	child, err := s.users.server(u.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	_, rpcErr, err := child.backend.Call(ctx, "model/list", json.RawMessage(`{"limit":10}`))
	if err != nil || rpcErr != nil {
		t.Fatalf("handshake failed: %v %v", rpcErr, err)
	}
	params, err := child.sanitizeUserParams("thread/start", json.RawMessage(`{"cwd":`+quote(b.insideCWD)+`}`))
	if err != nil {
		t.Fatal(err)
	}
	result, rpcErr, err := child.backend.Call(ctx, "thread/start", params)
	if err != nil || rpcErr != nil {
		t.Fatalf("restricted thread failed: %v %v", rpcErr, err)
	}
	var started map[string]any
	_ = json.Unmarshal(result, &started)
	if started["activePermissionProfile"] == nil {
		t.Fatalf("missing enforced profile: %s", result)
	}
	if started["serviceTier"] != nil && started["serviceTier"] != "default" {
		t.Fatalf("standard tier inherited privileged default: %v", started["serviceTier"])
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("shared MCP started for a restricted user")
	}
}

func TestNamedUserSandboxFilesystem(t *testing.T) {
	if os.Getenv("CODEX_REMOTE_TEST_ISOLATION") != "1" {
		t.Skip("set CODEX_REMOTE_TEST_ISOLATION=1 for native sandbox verification")
	}
	s, b, _ := newTestServerInstance(t, false)
	s.cfg.CodexHome = filepath.Join(filepath.Dir(b.insideCWD), ".codex")
	s.cfg.User = &User{ID: "sandbox", Revision: "1", Workspaces: []string{b.insideCWD}, Access: "workspace-write"}
	if err := os.WriteFile(filepath.Join(b.insideCWD, "allowed.txt"), []byte("allowed"), 0600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(filepath.Dir(b.insideCWD), "private.txt")
	if err := os.WriteFile(secret, []byte("secret-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"read-only", "workspace-write"} {
		s.cfg.User.Access = mode
		config, err := s.userRuntimeConfig("test")
		if err != nil {
			t.Fatal(err)
		}
		data, err := toml.Marshal(map[string]any{"permissions": map[string]any{"test": config["permissions.test"]}})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(s.cfg.CodexHome, "config.toml"), data, 0600); err != nil {
			t.Fatal(err)
		}
		writeCheck := "! touch write-test"
		if mode == "workspace-write" {
			writeCheck = "touch write-test"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		command := exec.CommandContext(ctx, "codex", "sandbox", "-P", "test", "-C", b.insideCWD, "--", "/bin/sh", "-c", `test "$(cat allowed.txt)" = allowed && ! cat "$1" && ! cat "$2" && `+writeCheck, "check", secret, filepath.Join(s.cfg.CodexHome, "config.toml"))
		command.Env = append(os.Environ(), "CODEX_HOME="+s.cfg.CodexHome)
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s: %v %s", mode, err, output)
		}
		if strings.Contains(string(output), "secret-fixture") {
			t.Fatal("sandbox leaked private data")
		}
	}
}

type sharedTestBackend struct {
	*fakeBackend
	ids []string
}

func (b *sharedTestBackend) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *codex.RPCError, error) {
	if method == "thread/start" {
		id := fmt.Sprintf("owned-%d", len(b.ids)+1)
		b.ids = append(b.ids, id)
		data, _ := json.Marshal(map[string]any{"thread": map[string]any{"id": id, "cwd": b.insideCWD}})
		return data, nil, nil
	}
	if method == "thread/list" {
		data := []any{map[string]any{"id": "admin-history", "cwd": b.insideCWD}}
		for _, id := range b.ids {
			data = append(data, map[string]any{"id": id, "cwd": b.insideCWD})
		}
		raw, _ := json.Marshal(map[string]any{"data": data})
		return raw, nil, nil
	}
	return b.fakeBackend.Call(ctx, method, params)
}
func TestSharedBackendOwnershipEventsAndReplay(t *testing.T) {
	s, f, password := newTestServerInstance(t, false)
	backend := &sharedTestBackend{fakeBackend: f}
	s.backend = backend
	if err := s.EnableUsers(s.UserFactory()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob"} {
		s.users.users = append(s.users.users, User{ID: id, Revision: "1", Name: id, Access: "read-only", Workspaces: []string{f.insideCWD}, Permissions: []string{"chat", "files", "uploads", "requests"}})
	}
	a, err := s.users.server("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.users.server("bob", false)
	if err != nil {
		t.Fatal(err)
	}
	if a.backend.(*userBackend).root.backend != backend || b.backend.(*userBackend).root.backend != backend {
		t.Fatal("accounts must share one backend")
	}
	ctx := context.Background()
	first, _, err := a.backend.Call(ctx, "thread/start", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := b.backend.Call(ctx, "thread/start", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	aliceID, bobID := resultThreadID(first), resultThreadID(second)
	for _, test := range []struct {
		s          *Server
		own, other string
	}{{a, aliceID, bobID}, {b, bobID, aliceID}} {
		list, _, err := test.s.backend.Call(ctx, "thread/list", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(list), test.other) || strings.Contains(string(list), "admin-history") || !strings.Contains(string(list), test.own) {
			t.Fatal(string(list))
		}
		raw, _ := json.Marshal(map[string]any{"threadId": test.other})
		if _, _, err = test.s.backend.Call(ctx, "thread/read", raw); err == nil {
			t.Fatal("cross-user thread read allowed")
		}
	}
	event, _ := json.Marshal(map[string]any{"type": "notification", "method": "item/agentMessage/delta", "params": map[string]any{"threadId": aliceID, "delta": "alice private"}})
	s.Observe(event)
	if a.broker.LatestID() != 1 || b.broker.LatestID() != 0 {
		t.Fatal("event leaked across accounts")
	}
	for _, child := range []*Server{a, b} {
		w := httptest.NewRecorder()
		if !child.beginSubmission(w, "shared-request-id", "thread/start", json.RawMessage(`{}`)) {
			t.Fatal("cross-account submission collision")
		}
	}
	h := s.Handler()
	admin := loginCookie(t, h, password)
	list := doRequest(h, "POST", "/api/rpc", `{"method":"thread/list","params":{}}`, admin, "")
	if list.Code != 200 || !strings.Contains(list.Body.String(), aliceID) || !strings.Contains(list.Body.String(), bobID) || !strings.Contains(list.Body.String(), "admin-history") {
		t.Fatal("administrator must see every account's threads", list.Body.String())
	}
	// Ownership persists even when no per-account backend state survives.
	if err := s.EnableUsers(s.UserFactory()); err != nil {
		t.Fatal(err)
	}
	if s.users.owners[aliceID] != "alice" || s.users.owners[bobID] != "bob" {
		t.Fatal("thread ownership was not durable")
	}
}

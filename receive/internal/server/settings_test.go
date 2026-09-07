package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestAccountSwitchPreservesSharedConfigAndRefreshedOAuth(t *testing.T) {
	s, _, password := newTestServerInstance(t, false)
	s.cfg.CodexHome = t.TempDir()
	original := configPair{`model = "official"
model_provider = "openai"
[mcp_servers.test]
command = "node"
args = ["server.js"]
[plugins."plugin@test"]
enabled = true
[[hooks.Stop]]
matcher = ""
[[hooks.Stop.hooks]]
type = "command"
command = "echo done"
[features]
plugins = true
`, `{"tokens":{"access_token":"old","refresh_token":"refresh"}}`}
	if err := s.writeSettings(settingsSnapshot{configPair: original}); err != nil {
		t.Fatal(err)
	}
	cookie := loginCookie(t, s.Handler(), password)
	call := func(body map[string]any, expected int) {
		t.Helper()
		snap, err := s.readSettings()
		if err != nil {
			t.Fatal(err)
		}
		body["revision"] = settingsRevision(snap)
		data, _ := json.Marshal(body)
		response := doRequest(s.Handler(), http.MethodPost, "/api/codex/settings", string(data), cookie, "")
		if response.Code != expected {
			t.Fatalf("%d %s", response.Code, response.Body.String())
		}
	}
	call(map[string]any{"action": "profile", "name": "API", "config": "model_provider = 'third'\n[model_providers.third]\nbase_url = 'https://example.test/v1'\nname = 'Third'\nwire_api = 'responses'\n", "auth": `{"OPENAI_API_KEY":"test-only"}`}, 200)
	snap, _ := s.readSettings()
	apiID := snap.Store.Profiles[0].ID
	call(map[string]any{"action": "apply", "id": apiID}, 200)
	snap, _ = s.readSettings()
	var a, b map[string]any
	toml.Unmarshal([]byte(original.Config), &a)
	toml.Unmarshal([]byte(snap.Config), &b)
	for _, key := range []string{"mcp_servers", "plugins", "hooks", "features"} {
		if !reflect.DeepEqual(a[key], b[key]) {
			t.Fatalf("shared %s changed", key)
		}
	}
	officialID := snap.Store.Profiles[1].ID
	call(map[string]any{"action": "apply", "id": officialID}, 200)
	snap, _ = s.readSettings()
	if snap.Auth != original.Auth {
		t.Fatal("OAuth credentials lost")
	}
	refreshed := `{"tokens":{"access_token":"fresh","refresh_token":"new-refresh"}}`
	if err := os.WriteFile(filepath.Join(s.cfg.CodexHome, "auth.json"), []byte(refreshed), 0600); err != nil {
		t.Fatal(err)
	}
	call(map[string]any{"action": "apply", "id": apiID}, 200)
	call(map[string]any{"action": "apply", "id": officialID}, 200)
	snap, _ = s.readSettings()
	if snap.Auth != refreshed {
		t.Fatal("refreshed OAuth credentials were overwritten")
	}
	before := settingsRevision(snap)
	call(map[string]any{"action": "save", "config": "bad = [", "auth": "{}"}, 400)
	snap, _ = s.readSettings()
	if settingsRevision(snap) != before {
		t.Fatal("invalid config changed files")
	}
	response := doRequest(s.Handler(), http.MethodPost, "/api/codex/settings", `{"action":"save","revision":"stale","config":"","auth":"{}"}`, cookie, "")
	if response.Code != 409 {
		t.Fatal("stale write accepted")
	}
	for _, name := range []string{"config.toml", "auth.json", "remote-auth-profiles.json", "remote-auth-backup.json"} {
		info, err := os.Stat(s.settingsPath(name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private permissions: %s", name)
		}
	}
	// Startup recovery rolls back a half-written config/auth pair.
	data, _ := json.Marshal(snap)
	if err := privateWrite(s.settingsPath(".remote-auth-transaction.json"), data); err != nil {
		t.Fatal(err)
	}
	privateWrite(s.settingsPath("config.toml"), []byte("model='partial'"))
	if err := s.recoverSettings(); err != nil {
		t.Fatal(err)
	}
	restored, _ := s.readSettings()
	if settingsRevision(restored) != settingsRevision(snap) {
		t.Fatal("interrupted transaction lost settings")
	}
	response = doRequest(s.Handler(), http.MethodGet, "/api/codex/settings", "", nil, "")
	if response.Code != 401 {
		t.Fatal("secrets readable without auth")
	}
}

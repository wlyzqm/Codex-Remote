package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPasswordConfigAndSessionLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{\"password\":\"correct horse battery staple\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(path)
	if err != nil || config.Password != "correct horse battery staple" {
		t.Fatalf("load config: config=%#v err=%v", config, err)
	}
	if !EqualPassword(config.Password, "correct horse battery staple") || EqualPassword(config.Password, "wrong password") {
		t.Fatal("password comparison returned an unexpected result")
	}
	sessionKey, err := NewSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	cookie, err := IssueSession(sessionKey, now.Add(time.Hour))
	if err != nil || !ValidateSession(sessionKey, cookie, now) {
		t.Fatalf("session validation failed: %v", err)
	}
	if ValidateSession(sessionKey, cookie, now.Add(2*time.Hour)) {
		t.Fatal("expired session was accepted")
	}
	replacement, err := NewSessionKey()
	if err != nil || replacement == sessionKey {
		t.Fatalf("replace session key: %v", err)
	}
	if ValidateSession(replacement, cookie, now) {
		t.Fatal("old session survived a receiver session-key change")
	}
}

func TestConfigRejectsUnsafeOrAmbiguousInput(t *testing.T) {
	tests := map[string]struct {
		body string
		mode os.FileMode
	}{
		"world readable":   {body: `{"password":"correct horse battery staple"}`, mode: 0o644},
		"short password":   {body: `{"password":"too-short"}`, mode: 0o600},
		"outer whitespace": {body: `{"password":" correct horse battery staple "}`, mode: 0o600},
		"unknown field":    {body: `{"password":"correct horse battery staple","token":"legacy"}`, mode: 0o600},
		"trailing value":   {body: `{"password":"correct horse battery staple"} {}`, mode: 0o600},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(test.body), test.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(path); err == nil {
				t.Fatal("unsafe config was accepted")
			}
		})
	}
}

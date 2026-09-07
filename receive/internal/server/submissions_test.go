package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-remote/internal/policy"
)

func TestSubmissionReplayAndUnknownResultNeverRepeatExecution(t *testing.T) {
	s, backend, password := newTestServerInstance(t, false)
	cookie := loginCookie(t, s.Handler(), password)
	body := `{"clientRequestId":"test-submission-0001","method":"turn/start","params":{"threadId":"inside","input":[{"type":"text","text":"continue"}]}}`
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/api/rpc", strings.NewReader(body)).WithContext(ctx)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("browser disconnect canceled submission: %d %s", w.Code, w.Body.String())
	}
	// Restart the HTTP receiver while retaining its on-disk submission records.
	restarted, err := New(s.cfg, backend, s.broker)
	if err != nil {
		t.Fatal(err)
	}
	replay := doRequest(restarted.Handler(), http.MethodPost, "/api/rpc", body, cookie, "")
	if replay.Code != http.StatusOK || !bytes.Contains(replay.Body.Bytes(), []byte(`"turn-1"`)) {
		t.Fatalf("saved result not replayed: %d %s", replay.Code, replay.Body.String())
	}
	mismatch := doRequest(restarted.Handler(), http.MethodPost, "/api/rpc", strings.Replace(body, "continue", "different", 1), cookie, "")
	if mismatch.Code != http.StatusConflict {
		t.Fatalf("changed input reused submission: %d", mismatch.Code)
	}
	backend.turnError = errors.New("response connection lost")
	unknownBody := strings.Replace(body, "0001", "0002", 1)
	unknown := doRequest(restarted.Handler(), http.MethodPost, "/api/rpc", unknownBody, cookie, "")
	if unknown.Code != http.StatusBadGateway {
		t.Fatalf("expected uncertain result, got %d", unknown.Code)
	}
	backend.turnError = nil
	unknown = doRequest(restarted.Handler(), http.MethodPost, "/api/rpc", unknownBody, cookie, "")
	if unknown.Code != http.StatusConflict || !strings.Contains(unknown.Body.String(), "submission_unknown") {
		t.Fatalf("uncertain request was retried: %d %s", unknown.Code, unknown.Body.String())
	}
	calls := 0
	for _, method := range backend.calls {
		if method == "turn/start" {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("duplicate execution: got %d calls, want 2 distinct submissions", calls)
	}
}

func TestProjectFilesIgnoreExtraGatesAfterLogin(t *testing.T) {
	s, backend, password := newTestServerInstance(t, false)
	paths, err := policy.NewPaths(nil)
	if err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(backend.insideCWD, "receiver-private.json")
	if err := os.WriteFile(private, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backend.insideCWD, "config.json"), []byte("project configuration"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(private, filepath.Join(backend.insideCWD, "alias.json")); err != nil {
		t.Fatal(err)
	}
	if err := paths.Protect([]string{private}); err != nil {
		t.Fatal(err)
	}
	s.cfg.Paths = paths
	cookie := loginCookie(t, s.Handler(), password)
	root := doRequest(s.Handler(), http.MethodGet, "/api/files?threadId=inside", "", cookie, "")
	var listing struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	if root.Code != http.StatusOK || json.Unmarshal(root.Body.Bytes(), &listing) != nil || len(listing.Entries) != 3 {
		t.Fatalf("workstation files hidden: %s", root.Body.String())
	}
	for _, path := range []string{"receiver-private.json", "alias.json"} {
		for _, suffix := range []string{"", "&download=1"} {
			for _, method := range []string{http.MethodGet, http.MethodHead} {
				response := doRequest(s.Handler(), method, "/api/files?threadId=inside&path="+path+suffix, "", cookie, "")
				if response.Code != http.StatusOK {
					t.Fatalf("%s %s%s: got %d", method, path, suffix, response.Code)
				}
			}
		}
	}
}

func TestUpgradeReplaysOldAcknowledgementWithoutSubmitting(t *testing.T) {
	s, backend, password := newTestServerInstance(t, false)
	params := json.RawMessage(`{"threadId":"inside","input":[{"type":"text","text":"continue","text_elements":[]}],"approvalPolicy":"never","approvalsReviewer":"user","sandboxPolicy":{"type":"dangerFullAccess"}}`)
	id := "upgrade-submission-0001"
	path := s.submissionPath(id)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(submissionRecord{Fingerprint: legacySubmissionFingerprint("turn/start", params), Status: 200, Response: json.RawMessage(`{"result":{"turn":{"id":"already-started"}}}`)})
	if err := os.WriteFile(path, record, 0600); err != nil {
		t.Fatal(err)
	}
	cookie := loginCookie(t, s.Handler(), password)
	response := doRequest(s.Handler(), http.MethodPost, "/api/rpc", `{"clientRequestId":"upgrade-submission-0001","method":"turn/start","params":{"threadId":"inside","input":[{"type":"text","text":"continue"}]}}`, cookie, "")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "already-started") {
		t.Fatalf("old acknowledgement lost: %d %s", response.Code, response.Body.String())
	}
	for _, method := range backend.calls {
		if method == "turn/start" {
			t.Fatal("old submission executed again")
		}
	}
}

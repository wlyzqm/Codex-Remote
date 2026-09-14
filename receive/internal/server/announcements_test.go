package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-remote/internal/policy"
)

func TestAnnouncementsGateEachUserLoginOnce(t *testing.T) {
	s, backend, password := newTestServerInstance(t, false)
	if err := s.EnableUsers(func(u User, _, _ string, _ *policy.Paths, _ func(any)) (Backend, error) {
		return &fakeBackend{insideCWD: u.Workspaces[0]}, nil
	}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	admin := loginCookie(t, h, password)
	create := func(name, notice, password string) User {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"action": "save", "password": password, "user": User{Name: name, Announcement: notice, Workspaces: []string{backend.insideCWD}, Access: "read-only", Permissions: []string{"chat"}}})
		response := doRequest(h, "POST", "/api/users", string(body), admin, "")
		if response.Code != 200 {
			t.Fatal(response.Body.String())
		}
		var result struct{ User User }
		_ = json.Unmarshal(response.Body.Bytes(), &result)
		return result.User
	}
	aliceUser := create("Alice", "Alice 的公告\n<script>仅显示文字</script>", "alice-notice-password")
	create("Bob", "Bob 的公告", "bob-notice-password")
	create("Quiet", "   ", "quiet-notice-password")
	alice := loginCookie(t, h, "alice-notice-password")
	bob := loginCookie(t, h, "bob-notice-password")
	quiet := loginCookie(t, h, "quiet-notice-password")
	request := func(method, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	pending := func(want bool, text string, cookies ...*http.Cookie) {
		t.Helper()
		response := request("GET", "/api/session", cookies...)
		var result struct {
			AnnouncementPending bool `json:"announcementPending"`
			User                User `json:"user"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.AnnouncementPending != want || result.User.Announcement != text {
			t.Fatalf("wrong announcement: %s", response.Body.String())
		}
	}
	pending(true, aliceUser.Announcement, alice)
	pending(true, "Bob 的公告", bob)
	pending(false, "", quiet)
	pending(false, "", admin)
	pending(false, aliceUser.Announcement, admin, &http.Cookie{Name: "codex_remote_view", Value: aliceUser.ID})
	for _, path := range []string{"/api/status", "/api/directories", "/api/rpc", "/api/events"} {
		if r := request("GET", path, alice); r.Code != 403 || !strings.Contains(r.Body.String(), "announcement_required") {
			t.Fatalf("workstation available before acknowledgement: %s %d", path, r.Code)
		}
	}
	if request("POST", "/api/announcement/ack").Code != 401 || request("GET", "/api/announcement/ack", alice).Code != 405 {
		t.Fatal("acknowledgement must require authenticated POST")
	}
	if r := doRequest(h, "POST", "/api/announcement/ack", "{}", alice, "https://evil.test"); r.Code != 403 {
		t.Fatal("cross-origin acknowledgement accepted")
	}
	ack := request("POST", "/api/announcement/ack", alice)
	if ack.Code != 200 || len(ack.Result().Cookies()) != 1 {
		t.Fatalf("acknowledgement failed: %d %s", ack.Code, ack.Body.String())
	}
	receipt := ack.Result().Cookies()[0]
	if !receipt.HttpOnly || receipt.SameSite != http.SameSiteStrictMode {
		t.Fatal("receipt must use protected cookie")
	}
	pending(false, aliceUser.Announcement, alice, receipt)
	pending(false, aliceUser.Announcement, alice, receipt) // Reload/new tab, same login.
	if request("GET", "/api/status", alice, receipt).Code != 200 {
		t.Fatal("workstation still blocked after acknowledgement")
	}
	pending(true, "Bob 的公告", bob, receipt)
	pending(true, aliceUser.Announcement, alice, &http.Cookie{Name: announcementCookie, Value: "forged"})
	newLogin := loginCookie(t, h, "alice-notice-password")
	pending(true, aliceUser.Announcement, newLogin, receipt)
	if request("POST", "/api/users", alice, receipt).Code != 403 {
		t.Fatal("user can configure announcements")
	}
	body, _ := json.Marshal(map[string]any{"action": "save", "user": aliceUser})
	if doRequest(h, "POST", "/api/users", strings.Replace(string(body), "Alice 的公告", strings.Repeat("字", 10001), 1), admin, "").Code != 400 {
		t.Fatal("oversized announcement accepted")
	}
	if request("POST", "/api/logout", alice, receipt).Code != 200 {
		t.Fatal("logout failed")
	}
	if request("POST", "/api/announcement/ack", alice, receipt).Code != 401 {
		t.Fatal("revoked login can acknowledge")
	}
}

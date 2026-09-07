package server

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkstationHistorySurvivesNotificationRetirement(t *testing.T) {
	s, backend, password := newTestServerInstance(t, false)
	s.Observe([]byte(`{"type":"server_request","request":{"key":"request-1","method":"item/fileChange/requestApproval","params":{"threadId":"inside","reason":"确认修改"}}}`))
	s.Observe([]byte(`{"type":"server_request_answered","key":"request-1","decision":{"decision":"accept"}}`))
	s.Observe([]byte(`{"type":"notification","method":"turn/completed","params":{"threadId":"inside","turn":{"id":"done-1","status":"completed"}}}`))
	s.Observe([]byte(`{"type":"notification","method":"turn/completed","params":{"threadId":"inside","turn":{"id":"done-1","status":"completed"}}}`))
	restored, err := New(s.cfg, backend, s.broker)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored.activity.History) != 1 || restored.activity.History[0].Decision != "已允许" {
		t.Fatal("history lost")
	}
	cookie := loginCookie(t, restored.Handler(), password)
	if got := doRequest(restored.Handler(), http.MethodGet, "/api/push", "", cookie, ""); got.Code != 404 {
		t.Fatal("retired push endpoint remains")
	}
}

func TestUploadManagementRetainsFilesUntilExplicitDelete(t *testing.T) {
	s, _, password := newTestServerInstance(t, false)
	cookie := loginCookie(t, s.Handler(), password)
	path := filepath.Join(s.cfg.UploadRoot, strings.Repeat("a", 32)+"-notes.txt")
	if err := os.WriteFile(path, []byte("notes"), 0600); err != nil {
		t.Fatal(err)
	}
	result := doRequest(s.Handler(), http.MethodGet, "/api/uploads", "", cookie, "")
	if result.Code != 200 || !strings.Contains(result.Body.String(), "notes.txt") || strings.Contains(result.Body.String(), ".remote-state") {
		t.Fatal(result.Body.String())
	}
	result = doRequest(s.Handler(), http.MethodDelete, "/api/uploads?path="+url.QueryEscape(path), "", cookie, "")
	if result.Code != 200 {
		t.Fatal(result.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file was not removed")
	}
}

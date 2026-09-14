package server

import (
	"net/http"
	"time"

	"codex-remote/internal/auth"
)

const announcementCookie = "codex_remote_announcement"

// Bind the receipt to the signed login token, so a new login always needs a
// new receipt. No per-session in-memory registry or Codex process is needed.
func (s *Server) announcementKey(r *http.Request) string {
	cookie, _ := s.sessionCookie(r) // The user router has already authenticated it.
	return s.cfg.SessionKey + "\x00announcement\x00" + cookie.Value
}

func (s *Server) announcementPending(r *http.Request) bool {
	if s.cfg.User == nil || s.cfg.User.Announcement == "" {
		return false
	}
	cookie, err := r.Cookie(announcementCookie)
	return err != nil || !auth.ValidateSession(s.announcementKey(r), cookie.Value, time.Now())
}

func (s *Server) acknowledgeAnnouncement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "使用 POST")
		return
	}
	expires := time.Now().Add(s.cfg.SessionTTL)
	value, err := auth.IssueSession(s.announcementKey(r), expires)
	if err != nil {
		writeError(w, 500, "announcement_error", "确认未能保存，请重试")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: announcementCookie, Value: value, Path: "/", Expires: expires,
		MaxAge: int(s.cfg.SessionTTL.Seconds()), HttpOnly: true,
		Secure: s.isSecure(r), SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

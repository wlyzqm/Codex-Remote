package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"codex-remote/internal/auth"
	"codex-remote/internal/events"
	"codex-remote/internal/policy"
)

type User struct {
	Announcement string   `json:"announcement"`
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Disabled     bool     `json:"disabled"`
	Workspaces   []string `json:"workspaces"`
	Models       []string `json:"models"`
	Efforts      []string `json:"efforts"`
	ServiceTiers []string `json:"serviceTiers"`
	Permissions  []string `json:"permissions"`
	Access       string   `json:"access"`
	Network      bool     `json:"network"`
	Revision     string   `json:"revision"`
	PasswordHash string   `json:"passwordHash,omitempty"`
}

func (u User) public() User               { u.PasswordHash = ""; return u }
func (u User) account() string            { return u.ID + ":" + u.Revision }
func (u User) can(permission string) bool { return slices.Contains(u.Permissions, permission) }

type UserBackendFactory func(User, string, string, *policy.Paths, func(any)) (Backend, error)
type userManager struct {
	loginSlot     chan struct{}
	owners        map[string]string
	requestOwners map[string]string
	mu            sync.Mutex
	root          *Server
	users         []User
	servers       map[string]*Server
	factory       UserBackendFactory
}

func (s *Server) EnableUsers(factory UserBackendFactory) error {
	if err := os.MkdirAll(s.cfg.UserRoot, 0700); err != nil {
		return err
	}
	m := &userManager{root: s, servers: map[string]*Server{}, factory: factory, loginSlot: make(chan struct{}, 1), owners: map[string]string{}, requestOwners: map[string]string{}}
	data, err := os.ReadFile(filepath.Join(s.cfg.UserRoot, "users.json"))
	if err == nil {
		if err = json.Unmarshal(data, &m.users); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err = os.ReadFile(filepath.Join(s.cfg.UserRoot, "threads.json"))
	if err == nil {
		if err = json.Unmarshal(data, &m.owners); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.users = m
	return nil
}
func (m *userManager) identity(r *http.Request) (string, bool) {
	cookie, err := m.root.sessionCookie(r)
	if err != nil {
		return "", false
	}
	account, valid := auth.SessionAccount(m.root.cfg.SessionKey, cookie.Value, time.Now())
	if !valid || m.root.sessionRevoked(cookie.Value, time.Now()) {
		return "", false
	}
	if account == "" {
		return "", true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if !u.Disabled && u.account() == account {
			return u.ID, true
		}
	}
	return "", false
}
func (m *userManager) login(password string) (string, bool) {
	if auth.EqualPassword(m.root.cfg.Password, password) {
		return "", true
	}
	m.mu.Lock()
	candidates := append([]User(nil), m.users...)
	m.mu.Unlock()
	for _, u := range candidates {
		if !u.Disabled && auth.CheckPassword(u.PasswordHash, password) {
			return u.account(), true
		}
	}
	return "", false
}
func (m *userManager) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/login" || !strings.HasPrefix(r.URL.Path, "/api/") {
		m.root.mux.ServeHTTP(w, r)
		return
	}
	id, valid := m.identity(r)
	if !valid {
		if r.URL.Path == "/api/session" {
			writeJSON(w, 200, map[string]any{"authenticated": false})
			return
		}
		writeError(w, 401, "authentication_required", "需要登录")
		return
	}
	if !m.root.sameOrigin(r) {
		writeError(w, 403, "origin_not_allowed", "请求来源不被允许")
		return
	}
	admin := id == ""
	if r.URL.Path == "/api/users" {
		if !admin {
			writeError(w, 403, "permission_denied", "仅管理员可管理用户")
			return
		}
		m.handleUsers(w, r)
		return
	}
	if r.URL.Path == "/api/users/select" {
		if !admin {
			writeError(w, 403, "permission_denied", "仅管理员可切换工作站")
			return
		}
		m.selectUser(w, r)
		return
	}
	if r.URL.Path == "/api/logout" {
		if r.Method != "POST" {
			writeError(w, 405, "method_not_allowed", "使用 POST")
			return
		}
		m.root.handleLogout(w, r)
		return
	}
	if admin {
		if c, err := r.Cookie("codex_remote_view"); err == nil {
			id = c.Value
		}
	}
	target := m.root
	if id != "" {
		var err error
		target, err = m.server(id, admin)
		if err != nil {
			if admin {
				target = m.root
				http.SetCookie(w, &http.Cookie{Name: "codex_remote_view", Path: "/", MaxAge: -1, HttpOnly: true, Secure: m.root.isSecure(r), SameSite: http.SameSiteStrictMode})
			} else {
				writeError(w, 403, "account_unavailable", err.Error())
				return
			}
		}
	}
	expected := r.Header.Get("X-Remote-Account")
	if expected == "" {
		expected = r.URL.Query().Get("account")
	}
	actual := "admin"
	if target.cfg.User != nil {
		actual = target.cfg.User.ID
	}
	if expected != "" && expected != actual {
		writeError(w, 409, "account_changed", "账户已切换，请刷新页面")
		return
	}
	pending := !admin && target.announcementPending(r)
	if r.URL.Path == "/api/session" {
		user := User{ID: "admin", Name: "管理员"}
		if target.cfg.User != nil {
			user = target.cfg.User.public()
		}
		writeJSON(w, 200, map[string]any{"authenticated": true, "admin": admin, "user": user, "announcementPending": pending})
		return
	}
	if r.URL.Path == "/api/announcement/ack" {
		target.acknowledgeAnnouncement(w, r)
		return
	}
	if pending {
		writeError(w, 403, "announcement_required", "请先阅读系统公告并点击“我已悉知”")
		return
	}
	target.mux.ServeHTTP(w, r)
}
func (m *userManager) server(id string, admin bool) (*Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.ID != id {
			continue
		}
		if u.Disabled && !admin {
			return nil, errors.New("账户已停用")
		}
		if child := m.servers[id]; child != nil {
			return child, nil
		}
		paths, err := policy.NewPaths(u.Workspaces)
		if err != nil {
			return nil, err
		}
		if len(paths.Roots()) == 0 {
			return nil, errors.New("账户未配置工作区")
		}
		home := filepath.Join(m.root.cfg.UserRoot, id, "codex")
		uploads := filepath.Join(m.root.cfg.UserRoot, id, "uploads")
		for _, p := range []string{home, uploads} {
			if err = os.MkdirAll(p, 0700); err != nil {
				return nil, err
			}
		}
		broker := events.New(256, 256<<10)
		var child *Server
		backend, err := m.factory(u, home, uploads, paths, func(any) {})
		if err != nil {
			return nil, err
		}
		cfg := m.root.cfg
		cfg.CodexHome = m.root.cfg.CodexHome
		cfg.UploadRoot = uploads
		cfg.GeneratedImagesRoot = filepath.Join(home, "generated_images")
		cfg.Paths = paths
		cfg.User = &u
		child, err = New(cfg, backend, broker)
		if err != nil {
			if c, ok := backend.(interface{ Close() error }); ok {
				_ = c.Close()
			}
			return nil, err
		}
		child.users = m
		child.rpcSlots = m.root.rpcSlots
		if scoped, ok := backend.(*userBackend); ok {
			scoped.child = child
		}
		m.servers[id] = child
		return child, nil
	}
	return nil, errors.New("账户不存在，请返回管理员工作站")
}
func (m *userManager) handleUsers(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Method == "GET" {
		users := []User{}
		for _, u := range m.users {
			users = append(users, u.public())
		}
		writeJSON(w, 200, map[string]any{"data": users})
		return
	}
	if r.Method != "POST" {
		writeError(w, 405, "method_not_allowed", "使用 GET 或 POST")
		return
	}
	var body struct {
		Action   string `json:"action"`
		Password string `json:"password"`
		User     User   `json:"user"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, 400, "invalid_user", err.Error())
		return
	}
	u := body.User
	idx := -1
	for i, existing := range m.users {
		if existing.ID == u.ID {
			idx = i
			break
		}
	}
	next := append([]User(nil), m.users...)
	if body.Action == "delete" {
		if idx < 0 {
			writeError(w, 404, "user_not_found", "用户不存在")
			return
		}
		if u.Revision != m.users[idx].Revision {
			writeError(w, 409, "user_changed", "用户已被修改，请重新加载")
			return
		}
		next = append(next[:idx], next[idx+1:]...)
	} else if body.Action == "save" {
		if idx < 0 && u.ID != "" {
			writeError(w, 404, "user_not_found", "用户不存在")
			return
		}
		if idx >= 0 && u.Revision != m.users[idx].Revision {
			writeError(w, 409, "user_changed", "用户已被修改，请重新加载")
			return
		}
		if idx < 0 {
			if len(next) >= 50 {
				writeError(w, 400, "user_limit", "最多创建 50 个用户")
				return
			}
			key, err := auth.NewSessionKey()
			if err != nil {
				writeError(w, 500, "user_error", "无法生成用户标识")
				return
			}
			u.ID = key[:22]
		}
		if err := m.validate(&u); err != nil {
			writeError(w, 400, "invalid_user", err.Error())
			return
		}
		if body.Password == "" && idx >= 0 {
			u.PasswordHash = m.users[idx].PasswordHash
		} else {
			if auth.EqualPassword(m.root.cfg.Password, body.Password) {
				writeError(w, 400, "duplicate_password", "用户密码不能与管理员密码相同")
				return
			}
			for _, other := range m.users {
				if other.ID != u.ID && auth.CheckPassword(other.PasswordHash, body.Password) {
					writeError(w, 400, "duplicate_password", "该密码已被其他账户使用")
					return
				}
			}
			hash, err := auth.HashPassword(body.Password)
			if err != nil {
				writeError(w, 400, "invalid_password", err.Error())
				return
			}
			u.PasswordHash = hash
		}
		revision, err := auth.NewSessionKey()
		if err != nil {
			writeError(w, 500, "user_error", "无法生成修订标识")
			return
		}
		u.Revision = revision
		if idx < 0 {
			next = append(next, u)
		} else {
			next[idx] = u
		}
	} else {
		writeError(w, 400, "invalid_action", "未知操作")
		return
	}
	data, err := json.Marshal(next)
	if err == nil {
		err = privateWrite(filepath.Join(m.root.cfg.UserRoot, "users.json"), data)
	}
	if err != nil {
		writeError(w, 500, "user_save_failed", "用户配置保存失败")
		return
	}
	m.users = next
	if child := m.servers[u.ID]; child != nil {
		delete(m.servers, u.ID)
		m.mu.Unlock()
		if c, ok := child.backend.(interface{ Close() error }); ok {
			_ = c.Close()
		}
		m.mu.Lock()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user": u.public()})
}
func (m *userManager) validate(u *User) error {
	u.Announcement = strings.TrimSpace(u.Announcement)
	if len([]rune(u.Announcement)) > 10000 {
		return errors.New("系统公告最多 10000 字")
	}
	u.Name = strings.TrimSpace(u.Name)
	if len(u.Name) == 0 || len([]rune(u.Name)) > 80 {
		return errors.New("用户名需为 1 至 80 字")
	}
	if len(u.Workspaces) == 0 || len(u.Workspaces) > 32 {
		return errors.New("必须指定 1 至 32 个工作区")
	}
	paths, err := policy.NewPaths(u.Workspaces)
	if err != nil {
		return err
	}
	protected := []string{m.root.cfg.UserRoot, m.root.cfg.CodexHome, m.root.cfg.UploadRoot, m.root.cfg.PasswordFile, "/proc", "/dev", "/sys", "/etc", "/usr", "/bin", "/lib", "/lib64"}
	existing := []string{}
	for _, p := range protected {
		if p != "" {
			if _, e := os.Stat(p); e == nil {
				existing = append(existing, p)
			}
		}
	}
	if err = paths.Protect(existing); err != nil {
		return err
	}
	for _, root := range paths.Roots() {
		if _, err = paths.Check(root); err != nil {
			return errors.New("工作区不能包含系统目录、认证文件或账户私有资料")
		}
	}
	u.Workspaces = paths.Roots()
	if u.Access != "read-only" && u.Access != "workspace-write" {
		return errors.New("文件权限必须为只读或工作区读写")
	}
	for _, p := range u.Permissions {
		if !slices.Contains([]string{"chat", "files", "uploads", "requests", "goals", "review", "archive"}, p) {
			return fmt.Errorf("未知权限 %q", p)
		}
	}
	for _, e := range u.Efforts {
		if !slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, e) {
			return fmt.Errorf("未知思考强度 %q", e)
		}
	}
	for _, values := range [][]string{u.Models, u.Efforts, u.ServiceTiers} {
		if len(values) > 100 {
			return errors.New("允许值最多 100 项")
		}
		for _, v := range values {
			if v == "" || len(v) > 160 || strings.ContainsAny(v, "\r\n\x00") {
				return errors.New("设置范围包含无效值")
			}
		}
	}
	return nil
}
func (m *userManager) selectUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, 405, "method_not_allowed", "使用 POST")
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if decodeJSON(r, &body) != nil {
		writeError(w, 400, "invalid_user", "无效用户")
		return
	}
	if body.ID != "" {
		m.mu.Lock()
		found := false
		for _, u := range m.users {
			if u.ID == body.ID {
				found = true
			}
		}
		m.mu.Unlock()
		if !found {
			writeError(w, 404, "user_not_found", "用户不存在")
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: "codex_remote_view", Value: body.ID, Path: "/", HttpOnly: true, Secure: m.root.isSecure(r), SameSite: http.SameSiteStrictMode})
	writeJSON(w, 200, map[string]any{"ok": true})
}

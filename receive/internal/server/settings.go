package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// All other keys remain workstation-wide, including MCP, plugins, hooks and projects.
var accountKeys = strings.Fields("model_provider model_providers model review_model model_reasoning_effort model_reasoning_summary model_verbosity model_context_window model_auto_compact_token_limit service_tier forced_login_method forced_chatgpt_workspace_id cli_auth_credentials_store chatgpt_base_url")

type configPair struct {
	Config string `json:"config"`
	Auth   string `json:"auth"`
}
type accountProfile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	configPair
}
type profileStore struct {
	Active   string           `json:"active"`
	Profiles []accountProfile `json:"profiles"`
}
type settingsSnapshot struct {
	configPair
	Store profileStore `json:"store"`
}

func privateWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".codex-remote-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
func (s *Server) settingsPath(name string) string { return filepath.Join(s.cfg.CodexHome, name) }
func (s *Server) readSettings() (settingsSnapshot, error) {
	var snap settingsSnapshot
	if s.cfg.CodexHome == "" {
		return snap, errors.New("未配置 Codex 工作站目录")
	}
	for name, target := range map[string]*string{"config.toml": &snap.Config, "auth.json": &snap.Auth} {
		data, err := os.ReadFile(s.settingsPath(name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return snap, err
		}
		*target = string(data)
	}
	data, err := os.ReadFile(s.settingsPath("remote-auth-profiles.json"))
	if err == nil {
		err = json.Unmarshal(data, &snap.Store)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return snap, err
}
func settingsRevision(snap settingsSnapshot) string {
	data, _ := json.Marshal(snap)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
func validatePair(pair configPair) (map[string]any, error) {
	if len(pair.Config) > 1<<20 || len(pair.Auth) > 256<<10 {
		return nil, errors.New("配置文件超过上限（config.toml 1 MiB，auth.json 256 KiB）")
	}
	var config map[string]any
	if err := toml.Unmarshal([]byte(pair.Config), &config); err != nil {
		return nil, errors.New("config.toml 语法无效，请检查后重试")
	}
	if config == nil {
		config = map[string]any{}
	}
	if strings.TrimSpace(pair.Auth) != "" {
		var auth map[string]any
		if json.Unmarshal([]byte(pair.Auth), &auth) != nil || auth == nil {
			return nil, errors.New("auth.json 必须是有效 JSON 对象；留空表示移除文件认证")
		}
	}
	return config, nil
}
func profilePair(pair configPair) (configPair, error) {
	config, err := validatePair(pair)
	if err != nil {
		return pair, err
	}
	selected := map[string]any{}
	for _, key := range accountKeys {
		if value, ok := config[key]; ok {
			selected[key] = value
		}
	}
	data, err := toml.Marshal(selected)
	return configPair{string(data), pair.Auth}, err
}
func switchPair(current, selected configPair) (configPair, error) {
	config, err := validatePair(current)
	if err != nil {
		return current, err
	}
	profile, err := validatePair(selected)
	if err != nil {
		return current, err
	}
	for _, key := range accountKeys {
		delete(config, key)
		if value, ok := profile[key]; ok {
			config[key] = value
		}
	}
	data, err := toml.Marshal(config)
	return configPair{string(data), selected.Auth}, err
}
func (s *Server) writeSettings(snap settingsSnapshot) error {
	if err := privateWrite(s.settingsPath("config.toml"), []byte(snap.Config)); err != nil {
		return err
	}
	if strings.TrimSpace(snap.Auth) == "" {
		if err := os.Remove(s.settingsPath("auth.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if err := privateWrite(s.settingsPath("auth.json"), []byte(snap.Auth)); err != nil {
		return err
	}
	data, err := json.Marshal(snap.Store)
	if err != nil {
		return err
	}
	return privateWrite(s.settingsPath("remote-auth-profiles.json"), data)
}
func (s *Server) recoverSettings() error {
	if s.cfg.CodexHome == "" {
		return nil
	}
	path := s.settingsPath(".remote-auth-transaction.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var previous settingsSnapshot
	if err = json.Unmarshal(data, &previous); err != nil {
		return err
	}
	if err = s.writeSettings(previous); err != nil {
		return err
	}
	return os.Remove(path)
}
func (s *Server) commitSettings(previous, next settingsSnapshot) error {
	data, err := json.Marshal(previous)
	if err != nil {
		return err
	}
	// Keep the previous pair for manual recovery and a journal for interrupted writes.
	if err = privateWrite(s.settingsPath("remote-auth-backup.json"), data); err != nil {
		return err
	}
	journal := s.settingsPath(".remote-auth-transaction.json")
	if err = privateWrite(journal, data); err != nil {
		return err
	}
	if err = s.writeSettings(next); err != nil {
		if rollback := s.recoverSettings(); rollback != nil {
			return fmt.Errorf("保存失败且回滚未完成，请恢复 remote-auth-backup.json")
		}
		return err
	}
	return os.Remove(journal)
}
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	fail := func(status int, message string) { writeError(w, status, "settings_error", message) }
	if err := s.recoverSettings(); err != nil {
		fail(500, "上次配置写入未完成，无法恢复，请检查工作站备份")
		return
	}
	current, err := s.readSettings()
	if err != nil {
		fail(500, "无法读取工作站配置")
		return
	}
	if r.Method == http.MethodGet {
		pair := current.configPair
		if id := r.URL.Query().Get("id"); id != "" {
			found := false
			for _, p := range current.Store.Profiles {
				if p.ID == id {
					pair, err = switchPair(current.configPair, p.configPair)
					found = true
					break
				}
			}
			if !found {
				fail(404, "配置不存在")
				return
			}
			if err != nil {
				fail(400, err.Error())
				return
			}
		}
		profiles := []map[string]string{}
		for _, p := range current.Store.Profiles {
			profiles = append(profiles, map[string]string{"id": p.ID, "name": p.Name})
		}
		writeJSON(w, 200, map[string]any{"config": pair.Config, "auth": pair.Auth, "profiles": profiles, "active": current.Store.Active, "revision": settingsRevision(current), "home": s.cfg.CodexHome})
		return
	}
	var body struct {
		Action, ID, Name, Revision string
		configPair
	}
	if err := decodeJSON(r, &body); err != nil {
		fail(400, "无法读取配置请求")
		return
	}
	if body.Revision != settingsRevision(current) {
		fail(409, "工作站配置已被修改，请重新读取后再保存")
		return
	}
	next := current
	next.Store.Profiles = append([]accountProfile(nil), current.Store.Profiles...)
	index := -1
	for i, p := range next.Store.Profiles {
		if p.ID == body.ID {
			index = i
			break
		}
	}
	switch body.Action {
	case "save":
		if _, err = validatePair(body.configPair); err != nil {
			fail(400, err.Error())
			return
		}
		next.configPair = body.configPair
		if current.Store.Active != "" {
			for i, p := range next.Store.Profiles {
				if p.ID == current.Store.Active {
					next.Store.Profiles[i].configPair, _ = profilePair(next.configPair)
				}
			}
		}
	case "profile":
		name := strings.TrimSpace(body.Name)
		if name == "" || len([]rune(name)) > 80 {
			fail(400, "配置名称需为 1 至 80 字")
			return
		}
		pair, e := profilePair(body.configPair)
		if e != nil {
			fail(400, e.Error())
			return
		}
		if index >= 0 {
			next.Store.Profiles[index] = accountProfile{body.ID, name, pair}
		} else {
			if len(next.Store.Profiles) >= 50 {
				fail(400, "最多保存 50 个配置")
				return
			}
			id := fmt.Sprintf("profile-%d", time.Now().UnixNano())
			next.Store.Profiles = append(next.Store.Profiles, accountProfile{id, name, pair})
		}
		// Saving a profile does not write the active config/auth pair.
		data, _ := json.Marshal(next.Store)
		if err = privateWrite(s.settingsPath("remote-auth-profiles.json"), data); err != nil {
			fail(500, "无法保存配置档案")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	case "apply":
		if index < 0 {
			fail(404, "配置不存在")
			return
		}
		if current.Store.Active == body.ID {
			writeJSON(w, 200, map[string]any{"ok": true})
			return
		}
		selected := next.Store.Profiles[index].configPair
		// Capture refreshed OAuth tokens before switching away from the active account.
		oldPair, e := profilePair(current.configPair)
		if e != nil {
			fail(400, e.Error())
			return
		}
		if current.Store.Active == "" {
			next.Store.Profiles = append(next.Store.Profiles, accountProfile{fmt.Sprintf("previous-%d", time.Now().UnixNano()), "切换前的工作站配置", oldPair})
		} else {
			for i, p := range next.Store.Profiles {
				if p.ID == current.Store.Active {
					next.Store.Profiles[i].configPair = oldPair
				}
			}
		}
		next.configPair, err = switchPair(current.configPair, selected)
		if err != nil {
			fail(400, err.Error())
			return
		}
		next.Store.Active = body.ID
	case "delete":
		if index < 0 {
			fail(404, "配置不存在")
			return
		}
		if current.Store.Active == body.ID {
			fail(400, "请先切换到其他配置，再删除当前配置")
			return
		}
		next.Store.Profiles = append(next.Store.Profiles[:index], next.Store.Profiles[index+1:]...)
		data, _ := json.Marshal(next.Store)
		if err = privateWrite(s.settingsPath("remote-auth-profiles.json"), data); err != nil {
			fail(500, "无法删除配置")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	case "reload":
		backend, ok := s.backend.(interface{ Reload() error })
		if !ok {
			fail(501, "当前后端不支持手动重载")
			return
		}
		if err = backend.Reload(); err != nil {
			fail(409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	default:
		fail(400, "未知配置操作")
		return
	}
	if err = s.commitSettings(current, next); err != nil {
		fail(500, "无法保存配置，已尝试恢复原文件；请检查工作站权限和备份")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

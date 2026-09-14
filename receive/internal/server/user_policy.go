package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pelletier/go-toml/v2"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func (s *Server) allowEndpoint(r *http.Request) bool {
	u := s.cfg.User
	if u == nil {
		return true
	}
	switch r.URL.Path {
	case "/api/codex/settings":
		return false
	case "/api/files", "/api/files/archive", "/api/artifact":
		return u.can("files")
	case "/api/uploads":
		return u.can("uploads")
	case "/api/requests":
		return u.can("requests")
	}
	if strings.HasPrefix(r.URL.Path, "/api/requests/") {
		return u.can("requests")
	}
	return true
}
func (s *Server) sanitizeUserParams(method string, raw json.RawMessage) (json.RawMessage, error) {
	u := s.cfg.User
	if u == nil {
		return raw, nil
	}
	permission := ""
	switch method {
	case "thread/start", "thread/resume", "thread/fork", "thread/name/set", "thread/compact/start", "turn/start", "turn/steer", "turn/interrupt":
		permission = "chat"
	case "thread/archive", "thread/unarchive":
		permission = "archive"
	case "thread/goal/set", "thread/goal/clear":
		permission = "goals"
	case "review/start":
		permission = "review"
	}
	if permission != "" && !u.can(permission) {
		return nil, errors.New("当前账户没有此操作权限")
	}
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	if method == "thread/list" {
		if cwd, ok := params["cwd"].(string); ok {
			if _, err := s.cfg.Paths.Check(cwd); err != nil {
				return nil, err
			}
		}
	}
	launch := method == "thread/start" || method == "thread/resume" || method == "thread/fork"
	if !launch && method != "turn/start" {
		return raw, nil
	}
	for _, setting := range []struct {
		field   string
		allowed []string
	}{{"model", u.Models}, {"effort", u.Efforts}, {"serviceTier", u.ServiceTiers}} {
		if launch && setting.field == "effort" {
			continue
		}
		value, present := params[setting.field]
		normalized, _ := value.(string)
		if setting.field == "serviceTier" && (value == nil || normalized == "") {
			normalized = "default"
		}
		if len(setting.allowed) > 0 {
			if !present || value == nil || value == "" {
				normalized = setting.allowed[0]
			} else if !slices.Contains(setting.allowed, normalized) {
				return nil, fmt.Errorf("%s 不在管理员允许的范围内", setting.field)
			}
			if setting.field == "serviceTier" && normalized == "default" {
				params[setting.field] = nil
			} else {
				params[setting.field] = normalized
			}
		}
	}
	params["approvalPolicy"] = "never"
	params["approvalsReviewer"] = "user"

	profile := "remote-" + u.ID + "-" + u.Revision
	delete(params, "sandbox")
	delete(params, "sandboxPolicy")
	params["permissions"] = profile
	if launch {
		cwd, _ := params["cwd"].(string)
		config, err := s.userRuntimeConfig(profile, cwd)
		if err != nil {
			return nil, err
		}
		params["config"] = config
		params["allowProviderModelFallback"] = false
	}

	return json.Marshal(params)
}
func (s *Server) filterUserModels(raw json.RawMessage) (json.RawMessage, error) {
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	data, ok := result["data"].([]any)
	if !ok {
		return nil, errors.New("模型列表格式无效")
	}
	filtered := []any{}
	for _, entry := range data {
		model, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		id, _ := model["model"].(string)
		if id == "" {
			id, _ = model["id"].(string)
		}
		if len(s.cfg.User.Models) > 0 && !slices.Contains(s.cfg.User.Models, id) {
			continue
		}
		if len(s.cfg.User.Efforts) > 0 {
			efforts := []any{}
			for _, v := range asArray(model["supportedReasoningEfforts"]) {
				name, _ := v.(string)
				if obj, ok := v.(map[string]any); ok {
					name, _ = obj["reasoningEffort"].(string)
				}
				if slices.Contains(s.cfg.User.Efforts, name) {
					efforts = append(efforts, v)
				}
			}
			model["supportedReasoningEfforts"] = efforts
			model["defaultReasoningEffort"] = s.cfg.User.Efforts[0]
		}
		filtered = append(filtered, model)
	}
	result["data"] = filtered
	return json.Marshal(result)
}
func asArray(v any) []any { items, _ := v.([]any); return items }

// A named profile travels with this thread. It never changes the shared
// app-server's configuration or starts another Codex process.
func (s *Server) userRuntimeConfig(profile string, workingDirectory ...string) (map[string]any, error) {
	u := s.cfg.User
	fs := map[string]any{":root": "deny", ":minimal": "read", "/proc": "deny"}
	access := "read"
	if u.Access == "workspace-write" {
		access = "write"
	}
	for _, root := range u.Workspaces {
		fs[root] = access
		fs[filepath.Join(root, ".codex")] = "read"
	}
	fs[s.cfg.UploadRoot] = "read"
	config := map[string]any{
		"permissions." + profile: map[string]any{"filesystem": fs, "network": map[string]any{"enabled": u.Network}},
		"default_permissions":    profile, "approval_policy": "never",
		"project_doc_max_bytes": 0, "skills.include_instructions": false,
		"features.goals":         u.can("goals"),
		"developer_instructions": "", "notify": []string{}, "web_search": "disabled",
		"shell_environment_policy.inherit": "none", "shell_environment_policy.set": map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8"},
	}
	for _, feature := range []string{"multi_agent", "multi_agent_v2", "multi_agent_mode", "collab", "apps", "connectors", "plugins", "hooks", "codex_hooks", "plugin_hooks", "memories", "memory_tool", "shell_snapshot", "request_permissions_tool", "browser_use", "computer_use", "js_repl", "code_mode"} {
		config["features."+feature] = false
	}
	if len(u.Efforts) > 0 {
		config["model_reasoning_effort"] = u.Efforts[0]
	}
	if len(u.Models) > 0 {
		config["review_model"] = u.Models[0]
	}
	// Explicit entries override merged MCP definitions from the workstation and
	// each project config layer. Plugin/app integrations are disabled above.
	cwd := u.Workspaces[0]
	if len(workingDirectory) > 0 && workingDirectory[0] != "" {
		cwd = workingDirectory[0]
	}
	config["projects"] = map[string]any{cwd: map[string]string{"trust_level": "untrusted"}}
	disabledMCP := map[string]any{}
	cleanEnv := map[string]string{}
	sources := []string{filepath.Join(s.cfg.CodexHome, "config.toml"), "/etc/codex/config.toml"}
	for _, root := range append(append([]string{}, u.Workspaces...), cwd) {
		for dir := root; ; dir = filepath.Dir(dir) {
			sources = append(sources, filepath.Join(dir, ".codex", "config.toml"))
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	for _, source := range sources {
		data, err := os.ReadFile(source)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var values map[string]any
		if err = toml.Unmarshal(data, &values); err != nil {
			return nil, err
		}
		if servers, ok := values["mcp_servers"].(map[string]any); ok {
			for name := range servers {
				disabledMCP[name] = map[string]any{"enabled": false}
			}
		}
		if env, ok := values["shell_environment_policy"].(map[string]any); ok {
			if set, ok := env["set"].(map[string]any); ok {
				for key := range set {
					cleanEnv[key] = ""
				}
			}
		}
	}
	cleanEnv["PATH"] = "/usr/local/bin:/usr/bin:/bin"
	cleanEnv["LANG"] = "C.UTF-8"
	config["shell_environment_policy.set"] = cleanEnv
	config["mcp_servers"] = disabledMCP
	return config, nil
}

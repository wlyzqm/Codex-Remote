package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

var ErrInvalidServerResponse = errors.New("invalid server request response")

var remotelyHandledServerRequests = map[string]struct{}{
	"item/commandExecution/requestApproval": {},
	"item/fileChange/requestApproval":       {},
	"item/permissions/requestApproval":      {},
	"item/tool/requestUserInput":            {},
	"mcpServer/elicitation/request":         {},
	"execCommandApproval":                   {},
	"applyPatchApproval":                    {},
}

func isRemotelyHandledServerRequest(method string) bool {
	_, ok := remotelyHandledServerRequests[method]
	return ok
}

func serverRequestThreadID(params json.RawMessage) string {
	var request struct {
		ThreadID       string `json:"threadId"`
		ConversationID string `json:"conversationId"`
	}
	if json.Unmarshal(params, &request) != nil {
		return ""
	}
	if request.ThreadID != "" {
		return request.ThreadID
	}
	return request.ConversationID
}

func sanitizeServerResponse(request pendingServerRequest, raw json.RawMessage, checkPath func(string) error) (json.RawMessage, error) {
	return sanitizeServerResponseWithChecks(request, raw, checkPath, checkPath)
}

func sanitizeServerResponseWithChecks(request pendingServerRequest, raw json.RawMessage, checkWorkspace, checkTarget func(string) error) (json.RawMessage, error) {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, fmt.Errorf("%w: result must be valid JSON", ErrInvalidServerResponse)
	}
	switch request.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		decision, err := stringDecision(raw)
		if err != nil {
			return nil, err
		}
		if decision != "accept" && decision != "decline" && decision != "cancel" {
			return nil, fmt.Errorf("%w: only one-shot accept, decline, or cancel is allowed", ErrInvalidServerResponse)
		}
		if request.Method == "item/commandExecution/requestApproval" && !commandDecisionAvailable(request.Params, decision) {
			return nil, fmt.Errorf("%w: decision is not offered by app-server", ErrInvalidServerResponse)
		}
		if decision == "accept" && (checkWorkspace != nil || checkTarget != nil) {
			var safe bool
			var reason string
			if request.Method == "item/fileChange/requestApproval" {
				safe, reason = validateFileApprovalEvidenceWithChecks(request, time.Now(), checkWorkspace, checkTarget)
			} else {
				safe, reason = serverRequestAcceptanceWithChecks(request.Method, request.Params, checkWorkspace, checkTarget)
			}
			if !safe {
				return nil, fmt.Errorf("%w: %s", ErrInvalidServerResponse, reason)
			}
		}
		return json.Marshal(map[string]string{"decision": decision})

	case "execCommandApproval", "applyPatchApproval":
		decision, err := stringDecision(raw)
		if err != nil {
			return nil, err
		}
		var legacy any
		switch decision {
		case "accept":
			if safe, reason := serverRequestAcceptanceWithChecks(request.Method, request.Params, checkWorkspace, checkTarget); !safe {
				return nil, fmt.Errorf("%w: %s", ErrInvalidServerResponse, reason)
			}
			legacy = "approved"
		case "decline":
			legacy = map[string]any{"denied": map[string]string{"rejection": "Declined from Codex Remote"}}
		case "cancel":
			legacy = "abort"
		default:
			return nil, fmt.Errorf("%w: only one-shot accept, decline, or cancel is allowed", ErrInvalidServerResponse)
		}
		return json.Marshal(map[string]any{"decision": legacy})

	case "item/tool/requestUserInput":
		return sanitizeUserInputResponse(request.Params, raw)
	case "item/permissions/requestApproval":
		return sanitizePermissionsResponseWithChecks(request.Params, raw, checkWorkspace, checkTarget)
	case "mcpServer/elicitation/request":
		return sanitizeMcpElicitationResponse(request.Params, raw)
	default:
		return nil, fmt.Errorf("%w: unsupported server request method", ErrInvalidServerResponse)
	}
}

func serverRequestAcceptance(method string, params json.RawMessage, checkPath func(string) error) (bool, string) {
	return serverRequestAcceptanceWithChecks(method, params, checkPath, checkPath)
}

func serverRequestAcceptanceWithChecks(method string, params json.RawMessage, checkWorkspace, checkTarget func(string) error) (bool, string) {
	if checkWorkspace == nil && checkTarget == nil && isRemotelyHandledServerRequest(method) {
		return true, ""
	}
	switch method {
	case "item/tool/requestUserInput":
		return true, ""
	case "item/permissions/requestApproval":
		_, reason := requestedPermissionGrantWithChecks(params, checkWorkspace, checkTarget)
		return reason == "", reason
	case "mcpServer/elicitation/request":
		return validateMcpElicitationRequest(params)
	case "item/fileChange/requestApproval":
		return false, "当前 Codex 请求未携带完整文件清单，只能拒绝或取消"
	case "applyPatchApproval":
		if checkTarget == nil {
			return false, "接收端未配置文件授权检查"
		}
		var request struct {
			FileChanges map[string]json.RawMessage `json:"fileChanges"`
			GrantRoot   string                     `json:"grantRoot"`
		}
		if json.Unmarshal(params, &request) != nil || len(request.FileChanges) == 0 {
			return false, "文件变更清单无法验证"
		}
		for path := range request.FileChanges {
			if err := checkedApprovalPath(path, "", checkTarget); err != nil {
				return false, "文件变更超出允许目录"
			}
		}
		if request.GrantRoot != "" {
			if err := checkedApprovalPath(request.GrantRoot, "", checkTarget); err != nil {
				return false, "请求的写入范围超出允许目录"
			}
		}
		return true, ""
	case "item/commandExecution/requestApproval":
		return validateV2CommandApprovalWithChecks(params, checkWorkspace, checkTarget)
	case "execCommandApproval":
		return validateLegacyCommandApprovalWithChecks(params, checkWorkspace, checkTarget)
	default:
		return false, "请求类型未开放远程批准"
	}
}

func validateV2CommandApprovalWithChecks(params json.RawMessage, checkWorkspace, checkTarget func(string) error) (bool, string) {
	if checkWorkspace == nil || checkTarget == nil {
		return false, "接收端未配置命令授权检查"
	}
	type action struct {
		Type string  `json:"type"`
		Path *string `json:"path"`
	}
	type fsEntry struct {
		Path struct {
			Type string `json:"type"`
			Path string `json:"path"`
		} `json:"path"`
		Access string `json:"access"`
	}
	var request struct {
		Command        string   `json:"command"`
		CWD            string   `json:"cwd"`
		Actions        []action `json:"commandActions"`
		NetworkContext *struct {
			Host     string `json:"host"`
			Protocol string `json:"protocol"`
		} `json:"networkApprovalContext"`
		Additional *struct {
			Network *struct {
				Enabled *bool `json:"enabled"`
			} `json:"network"`
			FileSystem *struct {
				Read    []string  `json:"read"`
				Write   []string  `json:"write"`
				Entries []fsEntry `json:"entries"`
			} `json:"fileSystem"`
		} `json:"additionalPermissions"`
	}
	if json.Unmarshal(params, &request) != nil || request.Command == "" || request.CWD == "" {
		return false, "命令或工作目录无法验证"
	}
	if err := checkedApprovalPath(request.CWD, "", checkWorkspace); err != nil {
		return false, "命令工作目录超出允许范围"
	}
	if len(request.Actions) == 0 {
		return false, "命令缺少可验证的动作清单"
	}
	for _, action := range request.Actions {
		switch action.Type {
		case "read":
			if action.Path == nil || checkedApprovalPath(*action.Path, request.CWD, checkTarget) != nil {
				return false, "命令读取路径超出允许范围"
			}
		case "listFiles", "search":
			if action.Path != nil && *action.Path != "" && checkedApprovalPath(*action.Path, request.CWD, checkTarget) != nil {
				return false, "命令访问路径超出允许范围"
			}
		default:
			return false, "命令包含无法安全解析的动作"
		}
	}
	if request.Additional != nil && request.Additional.FileSystem != nil {
		fileSystem := request.Additional.FileSystem
		for _, path := range append(append([]string{}, fileSystem.Read...), fileSystem.Write...) {
			if checkedApprovalPath(path, request.CWD, checkTarget) != nil {
				return false, "附加文件权限超出允许范围"
			}
		}
		for _, entry := range fileSystem.Entries {
			if entry.Path.Type != "path" || entry.Path.Path == "" || checkedApprovalPath(entry.Path.Path, request.CWD, checkTarget) != nil {
				return false, "附加文件权限包含无法验证的路径"
			}
		}
	}
	networkEnabled := request.Additional != nil && request.Additional.Network != nil && request.Additional.Network.Enabled != nil && *request.Additional.Network.Enabled
	if networkEnabled {
		if request.NetworkContext == nil || request.NetworkContext.Host == "" || !validNetworkProtocol(request.NetworkContext.Protocol) {
			return false, "附加网络权限缺少明确目标"
		}
	}
	if request.NetworkContext != nil && (request.NetworkContext.Host == "" || !validNetworkProtocol(request.NetworkContext.Protocol)) {
		return false, "网络目标无法验证"
	}
	return true, ""
}

func validateLegacyCommandApprovalWithChecks(params json.RawMessage, checkWorkspace, checkTarget func(string) error) (bool, string) {
	if checkWorkspace == nil || checkTarget == nil {
		return false, "接收端未配置命令授权检查"
	}
	var request struct {
		Command []string `json:"command"`
		CWD     string   `json:"cwd"`
		Parsed  []struct {
			Type string  `json:"type"`
			Path *string `json:"path"`
		} `json:"parsedCmd"`
	}
	if json.Unmarshal(params, &request) != nil || len(request.Command) == 0 || request.CWD == "" || len(request.Parsed) == 0 {
		return false, "命令缺少可验证的动作清单"
	}
	if checkedApprovalPath(request.CWD, "", checkWorkspace) != nil {
		return false, "命令工作目录超出允许范围"
	}
	for _, action := range request.Parsed {
		switch action.Type {
		case "read":
			if action.Path == nil || checkedApprovalPath(*action.Path, request.CWD, checkTarget) != nil {
				return false, "命令读取路径超出允许范围"
			}
		case "list_files", "search":
			if action.Path != nil && *action.Path != "" && checkedApprovalPath(*action.Path, request.CWD, checkTarget) != nil {
				return false, "命令访问路径超出允许范围"
			}
		default:
			return false, "命令包含无法安全解析的动作"
		}
	}
	return true, ""
}

func checkedApprovalPath(path, cwd string, checkPath func(string) error) error {
	if path == "" {
		return errors.New("empty path")
	}
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return errors.New("relative path without cwd")
		}
		path = filepath.Join(cwd, path)
	}
	return checkPath(path)
}

func validNetworkProtocol(protocol string) bool {
	switch protocol {
	case "http", "https", "socks5Tcp", "socks5Udp":
		return true
	default:
		return false
	}
}

func stringDecision(raw json.RawMessage) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 1 {
		return "", fmt.Errorf("%w: response must contain only decision", ErrInvalidServerResponse)
	}
	var decision string
	if json.Unmarshal(fields["decision"], &decision) != nil || decision == "" {
		return "", fmt.Errorf("%w: decision must be a string", ErrInvalidServerResponse)
	}
	return decision, nil
}

func commandDecisionAvailable(params json.RawMessage, wanted string) bool {
	var request struct {
		Available []json.RawMessage `json:"availableDecisions"`
	}
	if json.Unmarshal(params, &request) != nil || request.Available == nil {
		return true
	}
	for _, raw := range request.Available {
		var decision string
		if json.Unmarshal(raw, &decision) == nil && decision == wanted {
			return true
		}
	}
	return false
}

func sanitizeUserInputResponse(params, raw json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Questions []struct {
			ID string `json:"id"`
		} `json:"questions"`
	}
	if json.Unmarshal(params, &request) != nil || len(request.Questions) == 0 {
		return nil, fmt.Errorf("%w: request questions are invalid", ErrInvalidServerResponse)
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(raw, &top) != nil || len(top) != 1 {
		return nil, fmt.Errorf("%w: response must contain only answers", ErrInvalidServerResponse)
	}
	var answers map[string]json.RawMessage
	if json.Unmarshal(top["answers"], &answers) != nil {
		return nil, fmt.Errorf("%w: answers must be an object", ErrInvalidServerResponse)
	}
	wanted := make(map[string]struct{}, len(request.Questions))
	canonical := make(map[string]any, len(request.Questions))
	totalBytes := 0
	for _, question := range request.Questions {
		if question.ID == "" {
			return nil, fmt.Errorf("%w: question id is empty", ErrInvalidServerResponse)
		}
		wanted[question.ID] = struct{}{}
		var fields map[string]json.RawMessage
		answerRaw, ok := answers[question.ID]
		if !ok || json.Unmarshal(answerRaw, &fields) != nil || len(fields) != 1 {
			return nil, fmt.Errorf("%w: every question needs exactly one answers field", ErrInvalidServerResponse)
		}
		var values []string
		if json.Unmarshal(fields["answers"], &values) != nil || len(values) == 0 {
			return nil, fmt.Errorf("%w: every question needs at least one answer", ErrInvalidServerResponse)
		}
		for _, value := range values {
			totalBytes += len(value)
		}
		canonical[question.ID] = map[string]any{"answers": values}
	}
	for id := range answers {
		if _, ok := wanted[id]; !ok {
			return nil, fmt.Errorf("%w: answer references an unknown question", ErrInvalidServerResponse)
		}
	}
	if totalBytes > 1<<20 {
		return nil, fmt.Errorf("%w: answers exceed 1 MiB", ErrInvalidServerResponse)
	}
	return json.Marshal(map[string]any{"answers": canonical})
}

type requestedPermissions struct {
	CWD         string `json:"cwd"`
	Permissions struct {
		Network *struct {
			Enabled *bool `json:"enabled"`
		} `json:"network"`
		FileSystem *struct {
			Read    []string                 `json:"read"`
			Write   []string                 `json:"write"`
			Entries []permissionSandboxEntry `json:"entries"`
		} `json:"fileSystem"`
	} `json:"permissions"`
}

type permissionSandboxEntry struct {
	Path struct {
		Type    string `json:"type"`
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
		Value   string `json:"value"`
	} `json:"path"`
	Access string `json:"access"`
}

// requestedPermissionGrant validates the complete request and builds the
// largest safe one-turn subset that the small remote client may grant. It
// deliberately excludes session-scoped grants, glob patterns and special
// paths: those are difficult to explain and audit from a phone screen.
func requestedPermissionGrant(params json.RawMessage, checkPath func(string) error) (map[string]any, string) {
	return requestedPermissionGrantWithChecks(params, checkPath, checkPath)
}

func requestedPermissionGrantWithChecks(params json.RawMessage, checkWorkspace, checkTarget func(string) error) (map[string]any, string) {
	if checkWorkspace == nil {
		return nil, "接收端未配置权限路径检查"
	}
	var request requestedPermissions
	if json.Unmarshal(params, &request) != nil || request.CWD == "" {
		return nil, "权限请求或工作目录无法验证"
	}
	if checkedApprovalPath(request.CWD, "", checkWorkspace) != nil {
		return nil, "权限请求的工作目录超出允许范围"
	}
	grant := make(map[string]any)
	requested := false
	if request.Permissions.Network != nil && request.Permissions.Network.Enabled != nil && *request.Permissions.Network.Enabled {
		grant["network"] = map[string]any{"enabled": true}
		requested = true
	}
	if fileSystem := request.Permissions.FileSystem; fileSystem != nil {
		if checkTarget == nil {
			return nil, "接收端未配置文件目标检查"
		}
		canonical := make(map[string]any)
		if len(fileSystem.Read) > 0 {
			for _, path := range fileSystem.Read {
				if checkedApprovalPath(path, request.CWD, checkTarget) != nil {
					return nil, "请求的文件读取权限超出允许范围"
				}
			}
			canonical["read"] = append([]string(nil), fileSystem.Read...)
			requested = true
		}
		if len(fileSystem.Write) > 0 {
			for _, path := range fileSystem.Write {
				if checkedApprovalPath(path, request.CWD, checkTarget) != nil {
					return nil, "请求的文件写入权限超出允许范围"
				}
			}
			canonical["write"] = append([]string(nil), fileSystem.Write...)
			requested = true
		}
		if len(fileSystem.Entries) > 0 {
			entries := make([]permissionSandboxEntry, 0, len(fileSystem.Entries))
			for _, entry := range fileSystem.Entries {
				if entry.Path.Type != "path" || entry.Path.Path == "" || (entry.Access != "read" && entry.Access != "write") {
					return nil, "权限请求包含无法安全验证的路径规则"
				}
				if checkedApprovalPath(entry.Path.Path, request.CWD, checkTarget) != nil {
					return nil, "请求的文件权限超出允许范围"
				}
				entries = append(entries, entry)
			}
			canonical["entries"] = entries
			requested = true
		}
		if len(canonical) > 0 {
			grant["fileSystem"] = canonical
		}
	}
	if !requested {
		return nil, "权限请求没有可授予的明确权限"
	}
	return grant, ""
}

func sanitizePermissionsResponseWithChecks(params, raw json.RawMessage, checkWorkspace, checkTarget func(string) error) (json.RawMessage, error) {
	decision, err := stringDecision(raw)
	if err != nil {
		return nil, err
	}
	permissions := map[string]any{}
	strictAutoReview := false
	switch decision {
	case "accept":
		var reason string
		if checkWorkspace == nil && checkTarget == nil {
			var request struct {
				Permissions map[string]any `json:"permissions"`
			}
			if err := json.Unmarshal(params, &request); err != nil {
				return nil, err
			}
			permissions = request.Permissions
		} else {
			permissions, reason = requestedPermissionGrantWithChecks(params, checkWorkspace, checkTarget)
		}
		if reason != "" {
			return nil, fmt.Errorf("%w: %s", ErrInvalidServerResponse, reason)
		}
		// Keep reviewing subsequent commands in this turn. A phone approval is
		// never translated into an unattended session-wide permission change.
		strictAutoReview = checkWorkspace != nil || checkTarget != nil
	case "decline", "cancel":
		// The protocol represents denial as an empty granted subset.
	default:
		return nil, fmt.Errorf("%w: only one-shot accept, decline, or cancel is allowed", ErrInvalidServerResponse)
	}
	response := map[string]any{"permissions": permissions, "scope": "turn"}
	if strictAutoReview {
		response["strictAutoReview"] = true
	}
	return json.Marshal(response)
}

type mcpElicitationRequest struct {
	Mode            string          `json:"mode"`
	Message         string          `json:"message"`
	URL             string          `json:"url"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
}

func validateMcpElicitationRequest(params json.RawMessage) (bool, string) {
	var request mcpElicitationRequest
	if json.Unmarshal(params, &request) != nil || request.Message == "" || len(request.Message) > 64<<10 {
		return false, "MCP 请求内容无法验证"
	}
	switch request.Mode {
	case "url":
		parsed, err := url.Parse(request.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || len(request.URL) > 8192 {
			return false, "MCP URL 必须是无内嵌凭据的 HTTPS 地址"
		}
		return true, ""
	case "form":
		if err := validateMcpFormSchema(request.RequestedSchema); err != nil {
			return false, err.Error()
		}
		return true, ""
	case "openai/form":
		return false, "扩展 MCP 表单未在轻量远程端启用"
	default:
		return false, "未知的 MCP 请求模式"
	}
}

func sanitizeMcpElicitationResponse(params, raw json.RawMessage) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) == 0 {
		return nil, fmt.Errorf("%w: MCP response must be an object", ErrInvalidServerResponse)
	}
	for field := range fields {
		if field != "action" && field != "content" && field != "_meta" {
			return nil, fmt.Errorf("%w: unknown MCP response field %s", ErrInvalidServerResponse, field)
		}
	}
	var action string
	if json.Unmarshal(fields["action"], &action) != nil {
		return nil, fmt.Errorf("%w: MCP action is required", ErrInvalidServerResponse)
	}
	if meta, ok := fields["_meta"]; ok && string(meta) != "null" {
		return nil, fmt.Errorf("%w: client MCP metadata is not exposed remotely", ErrInvalidServerResponse)
	}
	if action == "decline" || action == "cancel" {
		if content, ok := fields["content"]; ok && string(content) != "null" {
			return nil, fmt.Errorf("%w: declined MCP request cannot include content", ErrInvalidServerResponse)
		}
		return json.Marshal(map[string]any{"action": action, "content": nil, "_meta": nil})
	}
	if action != "accept" {
		return nil, fmt.Errorf("%w: MCP action must be accept, decline, or cancel", ErrInvalidServerResponse)
	}
	if safe, reason := validateMcpElicitationRequest(params); !safe {
		return nil, fmt.Errorf("%w: %s", ErrInvalidServerResponse, reason)
	}
	var request mcpElicitationRequest
	_ = json.Unmarshal(params, &request)
	content := fields["content"]
	if request.Mode == "url" {
		if len(content) > 0 && string(content) != "null" {
			return nil, fmt.Errorf("%w: URL confirmation cannot include form content", ErrInvalidServerResponse)
		}
		return json.Marshal(map[string]any{"action": "accept", "content": nil, "_meta": nil})
	}
	if len(content) == 0 || string(content) == "null" {
		return nil, fmt.Errorf("%w: accepted MCP form requires content", ErrInvalidServerResponse)
	}
	canonical, err := validateMcpFormContent(request.RequestedSchema, content)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidServerResponse, err)
	}
	return json.Marshal(map[string]any{"action": "accept", "content": canonical, "_meta": nil})
}

type mcpFormSchema struct {
	Type       string                     `json:"type"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

func validateMcpFormSchema(raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return errors.New("MCP 表单 schema 大小无效")
	}
	var schema mcpFormSchema
	if json.Unmarshal(raw, &schema) != nil || schema.Type != "object" || len(schema.Properties) == 0 || len(schema.Properties) > 64 {
		return errors.New("MCP 表单 schema 不是受支持的对象")
	}
	for name, property := range schema.Properties {
		if name == "" || len(name) > 256 {
			return errors.New("MCP 表单包含无效字段名")
		}
		if err := validateMcpProperty(property, nil, false); err != nil {
			return fmt.Errorf("MCP 表单字段 %q 无效: %w", name, err)
		}
	}
	seen := make(map[string]struct{}, len(schema.Required))
	for _, name := range schema.Required {
		if _, ok := schema.Properties[name]; !ok {
			return errors.New("MCP 表单 required 引用了未知字段")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("MCP 表单 required 包含重复字段")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validateMcpFormContent(schemaRaw, contentRaw json.RawMessage) (map[string]any, error) {
	if len(contentRaw) > 1<<20 {
		return nil, errors.New("MCP 表单内容超过 1 MiB")
	}
	if err := validateMcpFormSchema(schemaRaw); err != nil {
		return nil, err
	}
	var schema mcpFormSchema
	_ = json.Unmarshal(schemaRaw, &schema)
	var content map[string]json.RawMessage
	if json.Unmarshal(contentRaw, &content) != nil || content == nil {
		return nil, errors.New("MCP 表单内容必须是对象")
	}
	for name := range content {
		if _, ok := schema.Properties[name]; !ok {
			return nil, fmt.Errorf("MCP 表单回答包含未知字段 %q", name)
		}
	}
	for _, name := range schema.Required {
		if _, ok := content[name]; !ok {
			return nil, fmt.Errorf("MCP 表单缺少必填字段 %q", name)
		}
	}
	canonical := make(map[string]any, len(content))
	for name, value := range content {
		var decoded any
		decoder := json.NewDecoder(strings.NewReader(string(value)))
		decoder.UseNumber()
		if decoder.Decode(&decoded) != nil {
			return nil, fmt.Errorf("MCP 表单字段 %q 不是有效 JSON", name)
		}
		if err := validateMcpProperty(schema.Properties[name], decoded, true); err != nil {
			return nil, fmt.Errorf("MCP 表单字段 %q: %w", name, err)
		}
		canonical[name] = decoded
	}
	return canonical, nil
}

func validateMcpProperty(raw json.RawMessage, value any, checkValue bool) error {
	var property struct {
		Type  string   `json:"type"`
		Enum  []string `json:"enum"`
		OneOf []struct {
			Const string `json:"const"`
		} `json:"oneOf"`
		Items *struct {
			Type  string   `json:"type"`
			Enum  []string `json:"enum"`
			AnyOf []struct {
				Const string `json:"const"`
			} `json:"anyOf"`
		} `json:"items"`
		MinLength *int     `json:"minLength"`
		MaxLength *int     `json:"maxLength"`
		Minimum   *float64 `json:"minimum"`
		Maximum   *float64 `json:"maximum"`
		MinItems  *int     `json:"minItems"`
		MaxItems  *int     `json:"maxItems"`
	}
	if json.Unmarshal(raw, &property) != nil {
		return errors.New("schema 无法解析")
	}
	switch property.Type {
	case "string":
		allowed := property.Enum
		for _, item := range property.OneOf {
			if item.Const == "" {
				return errors.New("枚举项为空")
			}
			allowed = append(allowed, item.Const)
		}
		if !checkValue {
			return validateLengthBounds(property.MinLength, property.MaxLength)
		}
		text, ok := value.(string)
		if !ok {
			return errors.New("必须是字符串")
		}
		length := len([]rune(text))
		if property.MinLength != nil && length < *property.MinLength || property.MaxLength != nil && length > *property.MaxLength {
			return errors.New("字符串长度超出 schema 范围")
		}
		if len(allowed) > 0 && !containsString(allowed, text) {
			return errors.New("值不在允许选项中")
		}
		return nil
	case "number", "integer":
		if property.Minimum != nil && property.Maximum != nil && *property.Minimum > *property.Maximum {
			return errors.New("数值范围无效")
		}
		if !checkValue {
			return nil
		}
		number, ok := value.(json.Number)
		if !ok {
			return errors.New("必须是数值")
		}
		numeric, err := number.Float64()
		if err != nil || math.IsInf(numeric, 0) || math.IsNaN(numeric) {
			return errors.New("数值无效")
		}
		if property.Type == "integer" && math.Trunc(numeric) != numeric {
			return errors.New("必须是整数")
		}
		if property.Minimum != nil && numeric < *property.Minimum || property.Maximum != nil && numeric > *property.Maximum {
			return errors.New("数值超出 schema 范围")
		}
		return nil
	case "boolean":
		if checkValue {
			if _, ok := value.(bool); !ok {
				return errors.New("必须是布尔值")
			}
		}
		return nil
	case "array":
		if property.Items == nil || (property.Items.Type != "" && property.Items.Type != "string") {
			return errors.New("只支持字符串选项数组")
		}
		allowed := append([]string(nil), property.Items.Enum...)
		for _, item := range property.Items.AnyOf {
			if item.Const == "" {
				return errors.New("枚举项为空")
			}
			allowed = append(allowed, item.Const)
		}
		if len(allowed) == 0 {
			return errors.New("数组缺少可选项")
		}
		if err := validateLengthBounds(property.MinItems, property.MaxItems); err != nil {
			return err
		}
		if !checkValue {
			return nil
		}
		values, ok := value.([]any)
		if !ok {
			return errors.New("必须是字符串数组")
		}
		if property.MinItems != nil && len(values) < *property.MinItems || property.MaxItems != nil && len(values) > *property.MaxItems {
			return errors.New("选项数量超出 schema 范围")
		}
		for _, rawValue := range values {
			text, ok := rawValue.(string)
			if !ok || !containsString(allowed, text) {
				return errors.New("数组包含无效选项")
			}
		}
		return nil
	default:
		return errors.New("只支持 string, number, integer, boolean 或枚举数组")
	}
}

func validateLengthBounds(minimum, maximum *int) error {
	if minimum != nil && (*minimum < 0 || *minimum > 1<<20) || maximum != nil && (*maximum < 0 || *maximum > 1<<20) {
		return errors.New("长度范围无效")
	}
	if minimum != nil && maximum != nil && *minimum > *maximum {
		return errors.New("最小长度大于最大长度")
	}
	return nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func serverRequestTimeout(method string, params json.RawMessage) time.Duration {
	const upperBound = 10 * time.Minute
	if method != "item/tool/requestUserInput" {
		return upperBound
	}
	var request struct {
		AutoResolutionMS *int64 `json:"autoResolutionMs"`
	}
	if json.Unmarshal(params, &request) != nil || request.AutoResolutionMS == nil || *request.AutoResolutionMS <= 0 {
		return upperBound
	}
	if *request.AutoResolutionMS >= int64(upperBound/time.Millisecond) {
		return upperBound
	}
	duration := time.Duration(*request.AutoResolutionMS) * time.Millisecond
	if duration < time.Second {
		return time.Second
	}
	if duration < upperBound {
		return duration
	}
	return upperBound
}

func timeoutResult(method string) (json.RawMessage, bool) {
	switch method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		return json.RawMessage(`{"decision":"cancel"}`), false
	case "execCommandApproval", "applyPatchApproval":
		return json.RawMessage(`{"decision":"abort"}`), false
	case "item/permissions/requestApproval":
		return json.RawMessage(`{"permissions":{},"scope":"turn"}`), false
	case "mcpServer/elicitation/request":
		return json.RawMessage(`{"action":"cancel","content":null,"_meta":null}`), false
	default:
		return nil, true
	}
}

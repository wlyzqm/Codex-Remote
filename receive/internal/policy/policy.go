package policy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var AllowedMethods = map[string]struct{}{
	"model/list":                      {},
	"modelProvider/capabilities/read": {},
	"thread/list":                     {},
	"thread/read":                     {},
	"thread/start":                    {},
	"thread/resume":                   {},
	"thread/fork":                     {},
	"thread/name/set":                 {},
	"thread/archive":                  {},
	"thread/unarchive":                {},
	"thread/goal/get":                 {},
	"thread/goal/set":                 {},
	"thread/goal/clear":               {},
	"thread/compact/start":            {},
	"review/start":                    {},
	"turn/start":                      {},
	"turn/steer":                      {},
	"turn/interrupt":                  {},
	"account/rateLimits/read":         {},
}

type Paths struct {
	roots     []string
	protected []string
}

func NewPaths(roots []string) (*Paths, error) {
	result := &Paths{}
	// An empty root list is the default unrestricted mode. Callers may select
	// any existing absolute workspace, while direct protected workspaces and
	// explicit file targets are still rejected below. Supplying one or more
	// roots opts back into the narrower legacy allowlist behavior.
	if len(roots) == 0 {
		return result, nil
	}
	seen := make(map[string]struct{})
	for _, root := range roots {
		canonical, err := canonicalDirectory(root)
		if err != nil {
			return nil, fmt.Errorf("allowed root %q: %w", root, err)
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		result.roots = append(result.roots, canonical)
	}
	return result, nil
}

func (p *Paths) Roots() []string {
	return append([]string(nil), p.roots...)
}

func (p *Paths) Unrestricted() bool {
	return len(p.roots) == 0
}

// Protect excludes receiver code, credentials and Codex state from direct
// workspace and explicit target checks. Restricted mode also rejects an
// ancestor workspace; unrestricted mode deliberately permits broad ancestors.
func (p *Paths) Protect(paths []string) error {
	seen := make(map[string]struct{}, len(p.protected))
	for _, existing := range p.protected {
		seen[existing] = struct{}{}
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		canonical, err := canonicalExistingPath(path)
		if err != nil {
			return fmt.Errorf("protected path %q: %w", path, err)
		}
		if _, ok := seen[canonical]; ok {
			continue
		}
		seen[canonical] = struct{}{}
		p.protected = append(p.protected, canonical)
	}
	return nil
}

func (p *Paths) Check(path string) (string, error) {
	canonical, err := canonicalDirectory(path)
	if err != nil {
		return "", err
	}
	return p.checkCanonical(canonical, true)
}

// CheckTarget validates an absolute file or directory target, including a
// not-yet-created final component, by resolving its nearest existing ancestor.
func (p *Paths) CheckTarget(path string) (string, error) {
	canonical, err := canonicalTarget(path)
	if err != nil {
		return "", err
	}
	return p.checkCanonical(canonical, false)
}

func (p *Paths) checkCanonical(canonical string, workspace bool) (string, error) {
	if len(p.roots) > 0 {
		allowed := false
		for _, root := range p.roots {
			relative, err := filepath.Rel(root, canonical)
			if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("workspace %q is outside the configured allowed roots", canonical)
		}
	}
	for _, protected := range p.protected {
		blocked := pathsOverlap(canonical, protected)
		// In default unrestricted mode, allow a broad project workspace such
		// as /root, /opt, or the receiver repository itself. Direct targets
		// inside protected credential/runtime paths remain blocked at this API
		// boundary, and the stricter ancestor rule remains when operators opt
		// into roots. This is not a sandbox for commands running inside a broad
		// workspace; the remote session still has the receiver user's authority.
		if workspace && p.Unrestricted() {
			blocked = pathContains(protected, canonical)
		}
		if blocked {
			return "", errors.New("workspace intersects a protected receiver or credential path")
		}
	}
	return canonical, nil
}

func canonicalTarget(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("target path must be absolute")
	}
	probe := filepath.Clean(path)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", err
		}
		suffix = append([]string{filepath.Base(probe)}, suffix...)
		probe = parent
	}
}

func canonicalExistingPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = absolute
	}
	return filepath.EvalSymlinks(filepath.Clean(path))
}

func pathsOverlap(left, right string) bool {
	return pathContains(left, right) || pathContains(right, left)
}

func pathContains(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func canonicalDirectory(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) {
		return "", errors.New("workspace path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("workspace path is not a directory")
	}
	return resolved, nil
}

// SanitizeLaunchParams validates cwd and prevents a remote browser from
// weakening the receiver's launch-time sandbox and approval boundaries.
func (p *Paths) SanitizeLaunchParams(method string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	allowed := map[string]map[string]struct{}{
		"thread/start": {
			"cwd": {}, "model": {}, "serviceTier": {},
		},
		"thread/resume": {
			"threadId": {}, "cwd": {}, "model": {},
		},
		"thread/fork": {
			"threadId": {}, "lastTurnId": {}, "beforeTurnId": {}, "cwd": {}, "model": {},
		},
	}[method]
	if allowed == nil {
		return nil, fmt.Errorf("launch policy does not support %s", method)
	}
	for field := range params {
		if _, ok := allowed[field]; !ok {
			return nil, fmt.Errorf("%s cannot be overridden remotely", field)
		}
	}
	if value, present := params["cwd"]; present {
		cwd, ok := value.(string)
		if !ok || cwd == "" {
			return nil, errors.New("cwd must be a non-empty absolute path")
		}
		canonical, err := p.Check(cwd)
		if err != nil {
			return nil, err
		}
		params["cwd"] = canonical
	}
	if method == "thread/start" {
		if _, ok := params["cwd"]; !ok {
			return nil, errors.New("thread/start requires an explicit cwd")
		}
	}
	if serviceTier, present := params["serviceTier"]; present && serviceTier != nil {
		value, ok := serviceTier.(string)
		if !ok || !isSafeServiceTier(value) {
			return nil, errors.New("serviceTier must be a short model-advertised identifier or null")
		}
	}
	params["approvalPolicy"] = "on-request"
	params["approvalsReviewer"] = "auto_review"
	params["sandbox"] = "workspace-write"
	if method == "thread/fork" {
		// A forked goal must not start an unattended continuation before the
		// receiver has validated and authorized the new thread id.
		params["deferGoalContinuation"] = true
	}
	return json.Marshal(params)
}

// SanitizeRichClientParams exposes a deliberately small subset of the richer
// app-server API. Every method uses an exact field allowlist so adding a method
// to AllowedMethods never implicitly exposes future protocol fields.
//
// Thread cwd ownership is checked by the HTTP server after this function has
// canonicalized the threadId. review/start is restricted to inline delivery:
// detached reviews create a second thread that the current receiver cannot
// validate and authorize atomically with the response.
func SanitizeRichClientParams(method string, raw json.RawMessage) (json.RawMessage, error) {
	params, err := decodeParamsObject(raw)
	if err != nil {
		return nil, err
	}

	switch method {
	case "modelProvider/capabilities/read":
		if err := rejectUnknownFields(params, nil); err != nil {
			return nil, err
		}
		return json.RawMessage(`{}`), nil

	case "thread/metadata/update":
		if err := rejectUnknownFields(params, map[string]struct{}{"threadId": {}, "isPinned": {}}); err != nil {
			return nil, err
		}
		threadID, err := requiredString(params, "threadId", 512)
		if err != nil {
			return nil, err
		}
		rawPinned, ok := params["isPinned"]
		if !ok {
			return nil, errors.New("isPinned is required")
		}
		var pinned *bool
		if err := json.Unmarshal(rawPinned, &pinned); err != nil || pinned == nil {
			return nil, errors.New("isPinned must be a boolean")
		}
		return json.Marshal(map[string]any{"threadId": threadID, "isPinned": *pinned})

	case "thread/compact/start":
		if err := rejectUnknownFields(params, map[string]struct{}{"threadId": {}}); err != nil {
			return nil, err
		}
		threadID, err := requiredString(params, "threadId", 512)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"threadId": threadID})

	case "review/start":
		return sanitizeReviewStart(params)

	default:
		return nil, fmt.Errorf("rich client policy does not support %s", method)
	}
}

// SanitizeStandardClientParams covers every remotely exposed method that is
// neither a launch, turn-input, nor rich-client operation. Keeping exact
// per-method field lists here makes additions to the app-server schema fail
// closed instead of silently widening the browser's RPC surface.
func SanitizeStandardClientParams(method string, raw json.RawMessage) (json.RawMessage, error) {
	params, err := decodeParamsObject(raw)
	if err != nil {
		return nil, err
	}
	clean := make(map[string]any)

	switch method {
	case "model/list":
		if err := rejectUnknownFields(params, fieldSet("cursor", "includeHidden", "limit")); err != nil {
			return nil, err
		}
		if err := copyOptionalString(clean, params, "cursor", 8192, false); err != nil {
			return nil, err
		}
		if err := copyOptionalBool(clean, params, "includeHidden", true); err != nil {
			return nil, err
		}
		if err := copyOptionalInt64(clean, params, "limit", 1, 200, true); err != nil {
			return nil, err
		}

	case "thread/list":
		if err := rejectUnknownFields(params, fieldSet(
			"ancestorThreadId", "archived", "cursor", "limit", "parentThreadId", "searchTerm",
			"sortDirection", "sortKey", "sourceKinds", "useStateDbOnly",
		)); err != nil {
			return nil, err
		}
		for _, field := range []string{"ancestorThreadId", "cursor", "parentThreadId"} {
			if err := copyOptionalString(clean, params, field, 8192, false); err != nil {
				return nil, err
			}
		}
		if err := copyOptionalString(clean, params, "searchTerm", 4096, true); err != nil {
			return nil, err
		}
		if err := copyOptionalBool(clean, params, "archived", true); err != nil {
			return nil, err
		}
		if err := copyOptionalBool(clean, params, "useStateDbOnly", false); err != nil {
			return nil, err
		}
		if err := copyOptionalInt64(clean, params, "limit", 1, 200, true); err != nil {
			return nil, err
		}
		if err := copyOptionalEnum(clean, params, "sortDirection", true, "asc", "desc"); err != nil {
			return nil, err
		}
		if err := copyOptionalEnum(clean, params, "sortKey", true, "created_at", "updated_at", "recency_at", "section_position"); err != nil {
			return nil, err
		}
		if err := copyThreadSourceKinds(clean, params); err != nil {
			return nil, err
		}
		if nonEmptyStringValue(clean["parentThreadId"]) && nonEmptyStringValue(clean["ancestorThreadId"]) {
			return nil, errors.New("parentThreadId and ancestorThreadId are mutually exclusive")
		}

	case "thread/read":
		if err := rejectUnknownFields(params, fieldSet("threadId", "includeTurns")); err != nil {
			return nil, err
		}
		threadID, err := requiredIdentifier(params, "threadId", 512)
		if err != nil {
			return nil, err
		}
		clean["threadId"] = threadID
		if err := copyOptionalBool(clean, params, "includeTurns", false); err != nil {
			return nil, err
		}

	case "thread/name/set":
		if err := rejectUnknownFields(params, fieldSet("threadId", "name")); err != nil {
			return nil, err
		}
		threadID, err := requiredIdentifier(params, "threadId", 512)
		if err != nil {
			return nil, err
		}
		name, err := requiredString(params, "name", 160)
		if err != nil || containsControl(name) {
			return nil, errors.New("name must be a non-empty single-line string of at most 160 bytes")
		}
		clean["threadId"], clean["name"] = threadID, name

	case "thread/archive", "thread/unarchive", "thread/goal/get", "thread/goal/clear":
		if err := rejectUnknownFields(params, fieldSet("threadId")); err != nil {
			return nil, err
		}
		threadID, err := requiredIdentifier(params, "threadId", 512)
		if err != nil {
			return nil, err
		}
		clean["threadId"] = threadID

	case "thread/goal/set":
		if err := rejectUnknownFields(params, fieldSet("threadId", "objective", "status", "tokenBudget")); err != nil {
			return nil, err
		}
		threadID, err := requiredIdentifier(params, "threadId", 512)
		if err != nil {
			return nil, err
		}
		clean["threadId"] = threadID
		if err := copyOptionalString(clean, params, "objective", 4000, true); err != nil {
			return nil, err
		}
		if err := copyOptionalEnum(clean, params, "status", true, "active", "paused", "blocked", "usageLimited", "budgetLimited", "complete"); err != nil {
			return nil, err
		}
		if err := copyOptionalInt64(clean, params, "tokenBudget", 1, int64(^uint64(0)>>1), true); err != nil {
			return nil, err
		}
		if len(clean) == 1 {
			return nil, errors.New("thread/goal/set requires objective, status, or tokenBudget")
		}

	case "turn/interrupt":
		if err := rejectUnknownFields(params, fieldSet("threadId", "turnId")); err != nil {
			return nil, err
		}
		threadID, err := requiredIdentifier(params, "threadId", 512)
		if err != nil {
			return nil, err
		}
		turnID, err := requiredIdentifier(params, "turnId", 512)
		if err != nil {
			return nil, err
		}
		clean["threadId"], clean["turnId"] = threadID, turnID

	case "account/rateLimits/read":
		if err := rejectUnknownFields(params, nil); err != nil {
			return nil, err
		}

	default:
		return nil, fmt.Errorf("standard client policy does not support %s", method)
	}
	return json.Marshal(clean)
}

func fieldSet(fields ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		result[field] = struct{}{}
	}
	return result
}

func requiredIdentifier(params map[string]json.RawMessage, field string, maxBytes int) (string, error) {
	value, err := requiredString(params, field, maxBytes)
	if err != nil {
		return "", err
	}
	if containsControl(value) {
		return "", fmt.Errorf("%s contains control characters", field)
	}
	return value, nil
}

func copyOptionalString(clean map[string]any, params map[string]json.RawMessage, field string, maxBytes int, allowEmpty bool) error {
	raw, ok := params[field]
	if !ok {
		return nil
	}
	if string(raw) == "null" {
		clean[field] = nil
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || len(value) > maxBytes || containsControl(value) || (!allowEmpty && value == "") {
		return fmt.Errorf("%s must be a valid string of at most %d bytes", field, maxBytes)
	}
	clean[field] = value
	return nil
}

func copyOptionalBool(clean map[string]any, params map[string]json.RawMessage, field string, nullable bool) error {
	raw, ok := params[field]
	if !ok {
		return nil
	}
	if string(raw) == "null" {
		if !nullable {
			return fmt.Errorf("%s cannot be null", field)
		}
		clean[field] = nil
		return nil
	}
	var value bool
	if json.Unmarshal(raw, &value) != nil {
		return fmt.Errorf("%s must be a boolean", field)
	}
	clean[field] = value
	return nil
}

func copyOptionalInt64(clean map[string]any, params map[string]json.RawMessage, field string, minimum, maximum int64, nullable bool) error {
	raw, ok := params[field]
	if !ok {
		return nil
	}
	if string(raw) == "null" {
		if !nullable {
			return fmt.Errorf("%s cannot be null", field)
		}
		clean[field] = nil
		return nil
	}
	var value int64
	if json.Unmarshal(raw, &value) != nil || value < minimum || value > maximum {
		return fmt.Errorf("%s must be an integer between %d and %d", field, minimum, maximum)
	}
	clean[field] = value
	return nil
}

func copyOptionalEnum(clean map[string]any, params map[string]json.RawMessage, field string, nullable bool, allowed ...string) error {
	raw, ok := params[field]
	if !ok {
		return nil
	}
	if string(raw) == "null" {
		if !nullable {
			return fmt.Errorf("%s cannot be null", field)
		}
		clean[field] = nil
		return nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return fmt.Errorf("%s must be a string", field)
	}
	for _, candidate := range allowed {
		if value == candidate {
			clean[field] = value
			return nil
		}
	}
	return fmt.Errorf("unsupported %s", field)
}

func copyThreadSourceKinds(clean map[string]any, params map[string]json.RawMessage) error {
	raw, ok := params["sourceKinds"]
	if !ok {
		return nil
	}
	if string(raw) == "null" {
		clean["sourceKinds"] = nil
		return nil
	}
	var values []string
	if json.Unmarshal(raw, &values) != nil || len(values) > 16 {
		return errors.New("sourceKinds must be an array with at most 16 entries")
	}
	allowed := fieldSet("cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact", "subAgentThreadSpawn", "subAgentOther", "unknown")
	seen := make(map[string]struct{}, len(values))
	canonical := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := allowed[value]; !ok {
			return errors.New("sourceKinds contains an unsupported source")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		canonical = append(canonical, value)
	}
	clean["sourceKinds"] = canonical
	return nil
}

func nonEmptyStringValue(value any) bool {
	text, ok := value.(string)
	return ok && text != ""
}

func sanitizeReviewStart(params map[string]json.RawMessage) (json.RawMessage, error) {
	if err := rejectUnknownFields(params, map[string]struct{}{"threadId": {}, "target": {}, "delivery": {}}); err != nil {
		return nil, err
	}
	threadID, err := requiredString(params, "threadId", 512)
	if err != nil {
		return nil, err
	}

	if rawDelivery, ok := params["delivery"]; ok && string(rawDelivery) != "null" {
		var delivery string
		if err := json.Unmarshal(rawDelivery, &delivery); err != nil || delivery != "inline" {
			return nil, errors.New("only inline review delivery is exposed remotely")
		}
	}

	var target map[string]json.RawMessage
	rawTarget, ok := params["target"]
	if !ok || json.Unmarshal(rawTarget, &target) != nil || target == nil {
		return nil, errors.New("review target must be an object")
	}
	targetType, err := requiredString(target, "type", 64)
	if err != nil {
		return nil, err
	}
	cleanTarget := map[string]any{"type": targetType}
	switch targetType {
	case "uncommittedChanges":
		if err := rejectUnknownFields(target, map[string]struct{}{"type": {}}); err != nil {
			return nil, err
		}

	case "baseBranch":
		if err := rejectUnknownFields(target, map[string]struct{}{"type": {}, "branch": {}}); err != nil {
			return nil, err
		}
		branch, err := requiredString(target, "branch", 512)
		if err != nil {
			return nil, err
		}
		if strings.HasPrefix(branch, "-") || containsControl(branch) {
			return nil, errors.New("review branch contains unsafe characters")
		}
		cleanTarget["branch"] = branch

	case "commit":
		if err := rejectUnknownFields(target, map[string]struct{}{"type": {}, "sha": {}, "title": {}}); err != nil {
			return nil, err
		}
		sha, err := requiredString(target, "sha", 64)
		if err != nil {
			return nil, err
		}
		if len(sha) < 7 || !isHexString(sha) {
			return nil, errors.New("review commit sha must be 7 to 64 hexadecimal characters")
		}
		cleanTarget["sha"] = sha
		if rawTitle, ok := target["title"]; ok && string(rawTitle) != "null" {
			title, err := requiredString(target, "title", 512)
			if err != nil {
				return nil, err
			}
			if containsControl(title) {
				return nil, errors.New("review title contains unsafe characters")
			}
			cleanTarget["title"] = title
		}

	case "custom":
		if err := rejectUnknownFields(target, map[string]struct{}{"type": {}, "instructions": {}}); err != nil {
			return nil, err
		}
		instructions, err := requiredString(target, "instructions", 1<<20)
		if err != nil {
			return nil, err
		}
		cleanTarget["instructions"] = instructions

	default:
		return nil, fmt.Errorf("unsupported review target %q", targetType)
	}

	return json.Marshal(map[string]any{
		"threadId": threadID,
		"delivery": "inline",
		"target":   cleanTarget,
	})
}

func decodeParamsObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil || params == nil {
		return nil, errors.New("params must be a JSON object")
	}
	return params, nil
}

func rejectUnknownFields(params map[string]json.RawMessage, allowed map[string]struct{}) error {
	for field := range params {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("%s cannot be overridden remotely", field)
		}
	}
	return nil
}

func requiredString(params map[string]json.RawMessage, field string, maxBytes int) (string, error) {
	raw, ok := params[field]
	if !ok {
		return "", fmt.Errorf("%s is required", field)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be a non-empty string", field)
	}
	if len(value) > maxBytes {
		return "", fmt.Errorf("%s exceeds the %d byte limit", field, maxBytes)
	}
	return value, nil
}

func containsControl(value string) bool {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return true
		}
	}
	return false
}

func isHexString(value string) bool {
	for _, character := range value {
		if !((character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f') ||
			(character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

// SanitizeTurnParams exposes text plus bounded inline raster images. Local file
// paths and remote URLs stay blocked at the generic JSON RPC boundary.
func SanitizeTurnParams(method string, raw json.RawMessage) (json.RawMessage, error) {
	return SanitizeTurnParamsWithLocalImages(method, raw, nil)
}

// SanitizeTurnParamsWithLocalImages permits only paths already issued and
// revalidated by the receiver's authenticated upload endpoint.
func SanitizeTurnParamsWithLocalImages(method string, raw json.RawMessage, checkLocalImage func(string) error) (json.RawMessage, error) {
	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	allowed := map[string]struct{}{
		"threadId": {}, "input": {}, "clientUserMessageId": {}, "model": {}, "effort": {}, "serviceTier": {}, "personality": {},
	}
	if method == "turn/steer" {
		allowed = map[string]struct{}{"threadId": {}, "expectedTurnId": {}, "input": {}}
	} else if method != "turn/start" {
		return nil, fmt.Errorf("turn policy does not support %s", method)
	}
	for field := range params {
		if _, ok := allowed[field]; !ok {
			return nil, fmt.Errorf("%s cannot be overridden remotely", field)
		}
	}
	var threadID string
	if json.Unmarshal(params["threadId"], &threadID) != nil || threadID == "" {
		return nil, errors.New("threadId is required")
	}
	var rawInputs []json.RawMessage
	if json.Unmarshal(params["input"], &rawInputs) != nil || len(rawInputs) == 0 {
		return nil, errors.New("at least one turn input is required")
	}
	inputs := make([]map[string]any, 0, len(rawInputs))
	totalText := 0
	for _, rawInput := range rawInputs {
		input, err := decodeParamsObject(rawInput)
		if err != nil {
			return nil, errors.New("turn input must be an object")
		}
		var inputType string
		if json.Unmarshal(input["type"], &inputType) != nil {
			return nil, errors.New("turn input type is required")
		}
		switch inputType {
		case "text":
			if err := rejectUnknownFields(input, map[string]struct{}{"type": {}, "text": {}, "text_elements": {}}); err != nil {
				return nil, err
			}
			var text string
			if json.Unmarshal(input["text"], &text) != nil {
				return nil, errors.New("text input must contain text")
			}
			totalText += len(text)
			if totalText > 1<<20 {
				return nil, errors.New("turn text exceeds 1 MiB limit")
			}
			inputs = append(inputs, map[string]any{"type": "text", "text": text, "text_elements": []any{}})
		case "image":
			if err := rejectUnknownFields(input, map[string]struct{}{"type": {}, "url": {}, "detail": {}}); err != nil {
				return nil, err
			}
			var imageURL string
			if json.Unmarshal(input["url"], &imageURL) != nil {
				return nil, errors.New("image input must contain a data URL")
			}
			if _, err := inlineImageBytes(imageURL); err != nil {
				return nil, err
			}
			detail := "auto"
			if rawDetail, present := input["detail"]; present && string(rawDetail) != "null" {
				if json.Unmarshal(rawDetail, &detail) != nil || (detail != "auto" && detail != "low" && detail != "high" && detail != "original") {
					return nil, errors.New("unsupported image detail")
				}
			}
			inputs = append(inputs, map[string]any{"type": "image", "url": imageURL, "detail": detail})
		case "localImage":
			if err := rejectUnknownFields(input, map[string]struct{}{"type": {}, "path": {}, "detail": {}}); err != nil {
				return nil, err
			}
			var path string
			if json.Unmarshal(input["path"], &path) != nil || path == "" || checkLocalImage == nil || checkLocalImage(path) != nil {
				return nil, errors.New("local image must reference a validated upload")
			}
			detail := "auto"
			if rawDetail, present := input["detail"]; present && string(rawDetail) != "null" {
				if json.Unmarshal(rawDetail, &detail) != nil || (detail != "auto" && detail != "low" && detail != "high" && detail != "original") {
					return nil, errors.New("unsupported image detail")
				}
			}
			inputs = append(inputs, map[string]any{"type": "localImage", "path": path, "detail": detail})
		default:
			return nil, errors.New("only text and validated image turn input is exposed remotely")
		}
	}
	result := map[string]any{"threadId": threadID, "input": inputs}
	if method == "turn/steer" {
		var expectedTurnID string
		if json.Unmarshal(params["expectedTurnId"], &expectedTurnID) != nil || expectedTurnID == "" {
			return nil, errors.New("expectedTurnId is required for turn/steer")
		}
		result["expectedTurnId"] = expectedTurnID
	} else if rawID, present := params["clientUserMessageId"]; present {
		var clientUserMessageID string
		if json.Unmarshal(rawID, &clientUserMessageID) != nil || clientUserMessageID == "" {
			return nil, errors.New("clientUserMessageId must be a non-empty string")
		}
		result["clientUserMessageId"] = clientUserMessageID
	}
	if method == "turn/start" {
		if rawModel, present := params["model"]; present {
			var model *string
			if err := json.Unmarshal(rawModel, &model); err != nil {
				return nil, errors.New("model must be a string or null")
			}
			if model == nil {
				result["model"] = nil
			} else if !isSafeModelID(*model) {
				return nil, errors.New("model must be a short model-advertised identifier")
			} else {
				result["model"] = *model
			}
		}
		if rawEffort, present := params["effort"]; present {
			var effort *string
			if err := json.Unmarshal(rawEffort, &effort); err != nil {
				return nil, errors.New("effort must be a string or null")
			}
			if effort == nil {
				result["effort"] = nil
			} else if !isSafeEffort(*effort) {
				return nil, errors.New("effort must be a short model-advertised identifier")
			} else {
				result["effort"] = *effort
			}
		}
		if rawServiceTier, present := params["serviceTier"]; present {
			var serviceTier *string
			if err := json.Unmarshal(rawServiceTier, &serviceTier); err != nil {
				return nil, errors.New("serviceTier must be a string or null")
			}
			if serviceTier == nil {
				result["serviceTier"] = nil
			} else if !isSafeServiceTier(*serviceTier) {
				return nil, errors.New("serviceTier must be a short model-advertised identifier")
			} else {
				result["serviceTier"] = *serviceTier
			}
		}
		if rawPersonality, present := params["personality"]; present {
			var personality *string
			if err := json.Unmarshal(rawPersonality, &personality); err != nil {
				return nil, errors.New("personality must be a string or null")
			}
			if personality == nil {
				result["personality"] = nil
			} else if *personality != "none" && *personality != "friendly" && *personality != "pragmatic" {
				return nil, errors.New("unsupported personality")
			} else {
				result["personality"] = *personality
			}
		}
	}
	return json.Marshal(result)
}

func inlineImageBytes(value string) (int, error) {
	header, payload, ok := strings.Cut(value, ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return 0, errors.New("image input must be an inline base64 data URL")
	}
	claimed := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
	if claimed != "image/png" && claimed != "image/jpeg" && claimed != "image/gif" && claimed != "image/webp" {
		return 0, errors.New("image input must be PNG, JPEG, GIF, or WebP")
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || len(decoded) == 0 {
		return 0, errors.New("image input contains invalid base64 data")
	}
	valid := map[string]bool{
		"image/png":  bytes.HasPrefix(decoded, []byte("\x89PNG\r\n\x1a\n")),
		"image/jpeg": len(decoded) >= 3 && decoded[0] == 0xff && decoded[1] == 0xd8 && decoded[2] == 0xff,
		"image/gif":  bytes.HasPrefix(decoded, []byte("GIF87a")) || bytes.HasPrefix(decoded, []byte("GIF89a")),
		"image/webp": len(decoded) >= 12 && string(decoded[:4]) == "RIFF" && string(decoded[8:12]) == "WEBP",
	}[claimed]
	if !valid {
		return 0, errors.New("image data does not match its declared format")
	}
	return len(decoded), nil
}

func isSafeModelID(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' || character == ':' {
			continue
		}
		return false
	}
	return true
}

func isSafeEffort(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' {
			continue
		}
		return false
	}
	return true
}

func isSafeServiceTier(value string) bool {
	return isSafeEffort(value)
}

func ThreadID(raw json.RawMessage) string {
	var params struct {
		ThreadID string `json:"threadId"`
	}
	_ = json.Unmarshal(raw, &params)
	return params.ThreadID
}

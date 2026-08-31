package codex

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func safeTestPath(path string) error {
	if path == "/safe" || len(path) > len("/safe/") && path[:len("/safe/")] == "/safe/" {
		return nil
	}
	return errors.New("outside /safe")
}

func TestApprovalResponsesAreOneShotAndTyped(t *testing.T) {
	request := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"command":"cat file","cwd":"/safe","commandActions":[{"type":"read","path":"/safe/file"}],"availableDecisions":["accept","acceptForSession","decline","cancel"]}`),
	}}
	result, err := sanitizeServerResponse(request, json.RawMessage(`{"decision":"accept"}`), safeTestPath)
	if err != nil || string(result) != `{"decision":"accept"}` {
		t.Fatalf("one-shot approval rejected: %s, %v", result, err)
	}
	for _, raw := range []string{
		`{"decision":"acceptForSession"}`,
		`{"decision":{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":{}}}}`,
		`{"decision":"accept","extra":true}`,
	} {
		if _, err := sanitizeServerResponse(request, json.RawMessage(raw), safeTestPath); !errors.Is(err, ErrInvalidServerResponse) {
			t.Fatalf("unsafe approval accepted: %s (%v)", raw, err)
		}
	}
}

func TestUnavailableCommandDecisionIsRejected(t *testing.T) {
	request := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"command":"cat file","cwd":"/safe","commandActions":[{"type":"read","path":"/safe/file"}],"availableDecisions":["decline"]}`),
	}}
	if _, err := sanitizeServerResponse(request, json.RawMessage(`{"decision":"accept"}`), safeTestPath); err == nil {
		t.Fatal("decision absent from availableDecisions was accepted")
	}
}

func TestLegacyApprovalIsTranslated(t *testing.T) {
	request := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "execCommandApproval",
		Params: json.RawMessage(`{"command":["cat","file"],"cwd":"/safe","parsedCmd":[{"type":"read","path":"/safe/file"}]}`),
	}}
	result, err := sanitizeServerResponse(request, json.RawMessage(`{"decision":"accept"}`), safeTestPath)
	if err != nil || string(result) != `{"decision":"approved"}` {
		t.Fatalf("legacy approval = %s, %v", result, err)
	}
	result, err = sanitizeServerResponse(request, json.RawMessage(`{"decision":"decline"}`), safeTestPath)
	if err != nil || !json.Valid(result) {
		t.Fatalf("legacy decline = %s, %v", result, err)
	}
}

func TestUserInputResponseMatchesQuestions(t *testing.T) {
	request := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"questions":[{"id":"choice"},{"id":"note"}]}`),
	}}
	valid := json.RawMessage(`{"answers":{"choice":{"answers":["yes"]},"note":{"answers":["done"]}}}`)
	if _, err := sanitizeServerResponse(request, valid, nil); err != nil {
		t.Fatalf("valid user input rejected: %v", err)
	}
	extra := json.RawMessage(`{"answers":{"choice":{"answers":["yes"]},"note":{"answers":["done"]},"other":{"answers":["x"]}}}`)
	if _, err := sanitizeServerResponse(request, extra, nil); err == nil {
		t.Fatal("answer for an unknown question was accepted")
	}
}

func TestServerRequestTimeoutIsBounded(t *testing.T) {
	short := serverRequestTimeout("item/tool/requestUserInput", json.RawMessage(`{"autoResolutionMs":2500}`))
	if short != 2500*time.Millisecond {
		t.Fatalf("short timeout = %s", short)
	}
	long := serverRequestTimeout("item/tool/requestUserInput", json.RawMessage(`{"autoResolutionMs":999999999}`))
	if long != 10*time.Minute {
		t.Fatalf("long timeout was not capped: %s", long)
	}
}

func TestApprovalCannotEscapeAllowedPaths(t *testing.T) {
	command := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"command":"cat /etc/shadow","cwd":"/safe","commandActions":[{"type":"read","path":"/etc/shadow"}],"availableDecisions":["accept","decline"]}`),
	}}
	if safe, _ := serverRequestAcceptance(command.Method, command.Params, safeTestPath); safe {
		t.Fatal("out-of-root command was marked safe to accept")
	}
	if _, err := sanitizeServerResponse(command, json.RawMessage(`{"decision":"accept"}`), safeTestPath); err == nil {
		t.Fatal("out-of-root command approval was accepted")
	}
	if _, err := sanitizeServerResponse(command, json.RawMessage(`{"decision":"decline"}`), safeTestPath); err != nil {
		t.Fatalf("out-of-root command could not be declined: %v", err)
	}

	file := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-1","grantRoot":"/safe"}`),
	}}
	if _, err := sanitizeServerResponse(file, json.RawMessage(`{"decision":"accept"}`), safeTestPath); err == nil {
		t.Fatal("v2 file request without a complete change list was accepted")
	}
}

func TestWorkspaceAndTargetChecksAreSeparated(t *testing.T) {
	var workspaceCalls []string
	var targetCalls []string
	checkWorkspace := func(path string) error {
		workspaceCalls = append(workspaceCalls, path)
		if path == "/root" {
			return nil
		}
		return errors.New("workspace rejected")
	}
	checkTarget := func(path string) error {
		targetCalls = append(targetCalls, path)
		if path == "/root/project/input" || path == "/root/project/output" {
			return nil
		}
		return errors.New("protected target")
	}

	command := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"command":"cat input","cwd":"/root","commandActions":[{"type":"read","path":"project/input"}],"availableDecisions":["accept","decline"]}`),
	}}
	if _, err := sanitizeServerResponseWithChecks(command, json.RawMessage(`{"decision":"accept"}`), checkWorkspace, checkTarget); err != nil {
		t.Fatalf("/root workspace with an allowed concrete target was rejected: %v", err)
	}
	if len(workspaceCalls) != 1 || workspaceCalls[0] != "/root" || len(targetCalls) != 1 || targetCalls[0] != "/root/project/input" {
		t.Fatalf("command check dispatch mismatch: workspace=%v target=%v", workspaceCalls, targetCalls)
	}

	command.Params = json.RawMessage(`{"command":"cat config","cwd":"/root","commandActions":[{"type":"read","path":".codex/config.toml"}],"availableDecisions":["accept","decline"]}`)
	if _, err := sanitizeServerResponseWithChecks(command, json.RawMessage(`{"decision":"accept"}`), checkWorkspace, checkTarget); err == nil {
		t.Fatal("protected /root/.codex command target was accepted")
	}
	if targetCalls[len(targetCalls)-1] != "/root/.codex/config.toml" {
		t.Fatalf("protected command target was not checked: %v", targetCalls)
	}

	permissions := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "item/permissions/requestApproval",
		Params: json.RawMessage(`{"cwd":"/root","permissions":{"fileSystem":{"write":["project/output"]}}}`),
	}}
	if _, err := sanitizeServerResponseWithChecks(permissions, json.RawMessage(`{"decision":"accept"}`), checkWorkspace, checkTarget); err != nil {
		t.Fatalf("/root permission cwd with an allowed concrete target was rejected: %v", err)
	}
	permissions.Params = json.RawMessage(`{"cwd":"/root","permissions":{"fileSystem":{"write":[".codex/config.toml"]}}}`)
	if _, err := sanitizeServerResponseWithChecks(permissions, json.RawMessage(`{"decision":"accept"}`), checkWorkspace, checkTarget); err == nil {
		t.Fatal("protected /root/.codex permission target was granted")
	}
}

func TestPermissionApprovalIsTurnScopedAndPathChecked(t *testing.T) {
	request := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "item/permissions/requestApproval",
		Params: json.RawMessage(`{
			"cwd":"/safe",
			"permissions":{
				"network":{"enabled":true},
				"fileSystem":{
					"read":["/safe/input"],
					"write":["/safe/output"],
					"entries":[{"path":{"type":"path","path":"/safe/cache"},"access":"write"}]
				}
			}
		}`),
	}}
	result, err := sanitizeServerResponse(request, json.RawMessage(`{"decision":"accept"}`), safeTestPath)
	if err != nil {
		t.Fatalf("safe permission grant rejected: %v", err)
	}
	var response struct {
		Scope            string         `json:"scope"`
		StrictAutoReview bool           `json:"strictAutoReview"`
		Permissions      map[string]any `json:"permissions"`
	}
	if json.Unmarshal(result, &response) != nil || response.Scope != "turn" || !response.StrictAutoReview || len(response.Permissions) != 2 {
		t.Fatalf("unexpected permission response: %s", result)
	}

	request.Params = json.RawMessage(`{"cwd":"/safe","permissions":{"fileSystem":{"write":["/etc"]}}}`)
	if _, err := sanitizeServerResponse(request, json.RawMessage(`{"decision":"accept"}`), safeTestPath); err == nil {
		t.Fatal("out-of-root permission was granted")
	}
	result, err = sanitizeServerResponse(request, json.RawMessage(`{"decision":"decline"}`), safeTestPath)
	if err != nil || string(result) != `{"permissions":{},"scope":"turn"}` {
		t.Fatalf("unsafe permission could not be declined: %s, %v", result, err)
	}
}

func TestMcpURLAndFormResponsesAreValidated(t *testing.T) {
	urlRequest := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "mcpServer/elicitation/request",
		Params: json.RawMessage(`{"threadId":"t","mode":"url","message":"Confirm login","url":"https://login.example/flow","elicitationId":"e"}`),
	}}
	result, err := sanitizeServerResponse(urlRequest, json.RawMessage(`{"action":"accept","content":null}`), safeTestPath)
	if err != nil || string(result) != `{"_meta":null,"action":"accept","content":null}` {
		t.Fatalf("valid URL elicitation rejected: %s, %v", result, err)
	}
	urlRequest.Params = json.RawMessage(`{"threadId":"t","mode":"url","message":"Bad","url":"javascript:alert(1)","elicitationId":"e"}`)
	if _, err := sanitizeServerResponse(urlRequest, json.RawMessage(`{"action":"accept"}`), safeTestPath); err == nil {
		t.Fatal("unsafe MCP URL was accepted")
	}
	if _, err := sanitizeServerResponse(urlRequest, json.RawMessage(`{"action":"cancel"}`), safeTestPath); err != nil {
		t.Fatalf("unsafe MCP URL could not be cancelled: %v", err)
	}

	formRequest := pendingServerRequest{PublicRequest: PublicRequest{
		Method: "mcpServer/elicitation/request",
		Params: json.RawMessage(`{
			"threadId":"t","mode":"form","message":"Choose",
			"requestedSchema":{"type":"object","properties":{
				"name":{"type":"string","minLength":2,"maxLength":8},
				"tier":{"type":"string","enum":["safe","fast"]},
				"count":{"type":"integer","minimum":1,"maximum":3},
				"flags":{"type":"array","items":{"type":"string","enum":["a","b"]},"minItems":1}
			},"required":["name","tier"]}
		}`),
	}}
	valid := json.RawMessage(`{"action":"accept","content":{"name":"codex","tier":"safe","count":2,"flags":["a"]}}`)
	if _, err := sanitizeServerResponse(formRequest, valid, safeTestPath); err != nil {
		t.Fatalf("valid MCP form rejected: %v", err)
	}
	for _, invalid := range []string{
		`{"action":"accept","content":{"tier":"safe"}}`,
		`{"action":"accept","content":{"name":"codex","tier":"other"}}`,
		`{"action":"accept","content":{"name":"codex","tier":"safe","extra":true}}`,
		`{"action":"accept","content":{"name":"codex","tier":"safe","count":2.5}}`,
	} {
		if _, err := sanitizeServerResponse(formRequest, json.RawMessage(invalid), safeTestPath); err == nil {
			t.Fatalf("invalid MCP form accepted: %s", invalid)
		}
	}
}

func TestRichServerRequestTimeoutsFailClosed(t *testing.T) {
	if result, useError := timeoutResult("item/permissions/requestApproval"); useError || string(result) != `{"permissions":{},"scope":"turn"}` {
		t.Fatalf("permission timeout = %s, error=%v", result, useError)
	}
	if result, useError := timeoutResult("mcpServer/elicitation/request"); useError || string(result) != `{"action":"cancel","content":null,"_meta":null}` {
		t.Fatalf("MCP timeout = %s, error=%v", result, useError)
	}
}

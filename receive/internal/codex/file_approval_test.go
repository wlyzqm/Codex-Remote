package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func fileChangeStartedParams(t *testing.T, threadID, turnID, itemID, path, movePath, diff string) json.RawMessage {
	t.Helper()
	kind := map[string]any{"type": "update"}
	if movePath != "" {
		kind["move_path"] = movePath
	}
	encoded, err := json.Marshal(map[string]any{
		"threadId":    threadID,
		"turnId":      turnID,
		"startedAtMs": int64(1234),
		"item": map[string]any{
			"type": "fileChange", "id": itemID, "status": "inProgress",
			"changes": []any{map[string]any{"path": path, "diff": diff, "kind": kind}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func stopPendingRequestTimers(client *Client) {
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, request := range client.requests {
		if request.Timer != nil {
			request.Timer.Stop()
		}
	}
}

func TestFileApprovalEvidenceUsesExactV2ItemShapeAndRedactsPublicDiff(t *testing.T) {
	now := time.Now()
	evidence, err := parseFileApprovalItemStarted(
		fileChangeStartedParams(t, "thread", "turn", "item", "/safe/new", "/safe/old", "super-secret-source"),
		9, now,
	)
	if err != nil {
		t.Fatalf("valid 0.149 item/started rejected: %v", err)
	}
	if evidence.Key != (fileApprovalKey{Generation: 9, ThreadID: "thread", TurnID: "turn", ItemID: "item"}) {
		t.Fatalf("evidence key = %#v", evidence.Key)
	}
	if len(evidence.Changes) != 1 || evidence.Changes[0].Diff != "super-secret-source" ||
		evidence.Changes[0].Kind.MovePath == nil || *evidence.Changes[0].Kind.MovePath != "/safe/old" {
		t.Fatalf("complete internal changes not retained: %#v", evidence.Changes)
	}
	public := publicFileApprovalChanges(evidence)
	if strings.Contains(string(public), "super-secret-source") || !strings.Contains(string(public), `"path":"/safe/new"`) ||
		!strings.Contains(string(public), `"move_path":"/safe/old"`) {
		t.Fatalf("public evidence was not safely redacted: %s", public)
	}
}

func TestFileApprovalEvidenceRejectsIncompleteOrAmbiguousChanges(t *testing.T) {
	tests := []string{
		`{"threadId":"t","turnId":"u","item":{"type":"fileChange","id":"i","status":"inProgress","changes":[]}}`,
		`{"threadId":"t","turnId":"u","startedAtMs":1,"item":{"type":"fileChange","id":"i","status":"inProgress","changes":[{"path":"/safe/a","kind":{"type":"add"}}]}}`,
		`{"threadId":"t","turnId":"u","startedAtMs":1,"item":{"type":"fileChange","id":"i","status":"inProgress","changes":[{"path":"/safe/a","diff":"x","kind":{"type":"mystery"}}]}}`,
		`{"threadId":"t","turnId":"u","startedAtMs":1,"item":{"type":"fileChange","id":"i","status":"inProgress","changes":[{"path":"/safe/a","diff":"x","kind":{"type":"add"},"unknownPath":"/etc/shadow"}]}}`,
	}
	for _, raw := range tests {
		if _, err := parseFileApprovalItemStarted(json.RawMessage(raw), 1, time.Now()); err == nil {
			t.Fatalf("ambiguous evidence accepted: %s", raw)
		}
	}
	oversized := fileChangeStartedParams(t, "t", "u", "i", "/safe/a", "", strings.Repeat("x", maxFileApprovalEntryBytes))
	if _, err := parseFileApprovalItemStarted(oversized, 1, time.Now()); err == nil {
		t.Fatal("oversized evidence accepted")
	}
}

func TestFileApprovalSeparatesWorkspaceAndTargetChecks(t *testing.T) {
	now := time.Now()
	checkWorkspace := func(path string) error {
		if path == "/root" {
			return nil
		}
		return errors.New("workspace rejected")
	}
	checkTarget := func(path string) error {
		if path == "/root/project/new" || path == "/root/project" {
			return nil
		}
		return errors.New("protected target")
	}
	makeRequest := func(path, grantRoot string) pendingServerRequest {
		evidence, err := parseFileApprovalItemStarted(fileChangeStartedParams(t, "thread", "turn", "item", path, "", "diff"), 7, now)
		if err != nil {
			t.Fatal(err)
		}
		evidence.CWD = "/root"
		params, err := json.Marshal(map[string]any{
			"threadId": "thread", "turnId": "turn", "itemId": "item", "grantRoot": grantRoot,
		})
		if err != nil {
			t.Fatal(err)
		}
		return pendingServerRequest{
			PublicRequest: PublicRequest{Method: "item/fileChange/requestApproval", Params: params},
			Generation:    7, FileApproval: evidence,
		}
	}

	allowed := makeRequest("project/new", "project")
	if safe, reason := validateFileApprovalEvidenceWithChecks(allowed, now, checkWorkspace, checkTarget); !safe {
		t.Fatalf("/root file approval with allowed targets was rejected: %s", reason)
	}
	protected := makeRequest(".codex/config.toml", ".codex")
	if safe, _ := validateFileApprovalEvidenceWithChecks(protected, now, checkWorkspace, checkTarget); safe {
		t.Fatal("protected /root/.codex file approval target was accepted")
	}
}

func TestV2FileApprovalChecksPathsAtDisplayAndAccept(t *testing.T) {
	client, transport, _ := newMultiAgentTestClient(7)
	client.AuthorizeThread("thread")
	var callsMu sync.Mutex
	var calls []string
	client.cfg.CheckPath = func(path string) error {
		callsMu.Lock()
		calls = append(calls, path)
		callsMu.Unlock()
		return safeTestPath(path)
	}
	client.handleNotification(rpcEnvelope{
		Method: "item/started",
		Params: fileChangeStartedParams(t, "thread", "turn", "item", "/safe/new", "/safe/old", "private diff"),
	}, transport, 7)
	requestParams := json.RawMessage(`{"threadId":"thread","turnId":"turn","itemId":"item","startedAtMs":1234,"grantRoot":"/safe"}`)
	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`31`), Method: "item/fileChange/requestApproval", Params: requestParams,
	}, transport, 7)
	pending := client.PendingRequests()
	if len(pending) != 1 || !pending[0].CanAccept || pending[0].PolicyReason != "" {
		stopPendingRequestTimers(client)
		t.Fatalf("safe correlated request not approvable: %#v", pending)
	}
	if strings.Contains(string(pending[0].FileChanges), "private diff") {
		stopPendingRequestTimers(client)
		t.Fatal("public pending request exposed diff content")
	}
	if err := client.Respond(context.Background(), pending[0].Key, json.RawMessage(`{"decision":"accept"}`)); err != nil {
		t.Fatalf("safe one-shot approval failed: %v", err)
	}
	callsMu.Lock()
	gotCalls := append([]string(nil), calls...)
	callsMu.Unlock()
	wantCalls := []string{"/safe/new", "/safe/old", "/safe", "/safe/new", "/safe/old", "/safe"}
	if fmt.Sprint(gotCalls) != fmt.Sprint(wantCalls) {
		t.Fatalf("path policy calls = %v, want display+accept %v", gotCalls, wantCalls)
	}
	client.mu.Lock()
	cacheCount, cacheBytes := len(client.fileApprovals), client.fileApprovalBytes
	client.mu.Unlock()
	if cacheCount != 0 || cacheBytes != 0 || len(client.PendingRequests()) != 0 {
		t.Fatalf("approved evidence retained: entries=%d bytes=%d", cacheCount, cacheBytes)
	}
	writes := transport.snapshotWrites()
	if len(writes) != 1 || !strings.Contains(string(writes[0]), `"result":{"decision":"accept"}`) || strings.Contains(string(writes[0]), "acceptForSession") {
		t.Fatalf("unexpected app-server response: %q", writes)
	}
}

func TestV2FileApprovalResolvesRelativePathsAgainstVerifiedThreadWorkspace(t *testing.T) {
	client, transport, _ := newMultiAgentTestClient(10)
	client.AuthorizeThread("thread")
	client.mu.Lock()
	client.putThreadWorkspaceLocked("thread", threadWorkspace{Generation: 10, CWD: "/safe/work"})
	client.mu.Unlock()
	client.handleNotification(rpcEnvelope{
		Method: "item/started",
		Params: fileChangeStartedParams(t, "thread", "turn", "item", "src/new.go", "src/old.go", "diff"),
	}, transport, 10)
	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`35`), Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","itemId":"item","startedAtMs":1234,"grantRoot":"."}`),
	}, transport, 10)
	pending := client.PendingRequests()
	if len(pending) != 1 || !pending[0].CanAccept {
		stopPendingRequestTimers(client)
		t.Fatalf("relative paths were not resolved through verified cwd: %#v", pending)
	}
	if err := client.Respond(context.Background(), pending[0].Key, json.RawMessage(`{"decision":"accept"}`)); err != nil {
		t.Fatalf("relative path approval failed: %v", err)
	}
}

func TestV2FileApprovalMissingOrUnauthorizedEvidenceCanOnlyBeDeclined(t *testing.T) {
	client, transport, _ := newMultiAgentTestClient(2)
	// This complete notification is intentionally received before authorization.
	client.handleNotification(rpcEnvelope{
		Method: "item/started",
		Params: fileChangeStartedParams(t, "thread", "turn", "item", "/safe/a", "", "diff"),
	}, transport, 2)
	client.AuthorizeThread("thread")
	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`41`), Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","itemId":"item","startedAtMs":1234}`),
	}, transport, 2)
	pending := client.PendingRequests()
	if len(pending) != 1 || pending[0].CanAccept || len(pending[0].FileChanges) != 0 {
		stopPendingRequestTimers(client)
		t.Fatalf("uncorrelated request gained evidence: %#v", pending)
	}
	if err := client.Respond(context.Background(), pending[0].Key, json.RawMessage(`{"decision":"accept"}`)); !errors.Is(err, ErrInvalidServerResponse) {
		stopPendingRequestTimers(client)
		t.Fatalf("uncorrelated accept error = %v", err)
	}
	if len(client.PendingRequests()) != 1 {
		t.Fatal("rejected accept consumed the request")
	}
	if err := client.Respond(context.Background(), pending[0].Key, json.RawMessage(`{"decision":"decline"}`)); err != nil {
		t.Fatalf("decline was not always available: %v", err)
	}
}

func TestV2FileApprovalReplacementOrExpiryPreventsAccept(t *testing.T) {
	client, transport, _ := newMultiAgentTestClient(4)
	client.AuthorizeThread("thread")
	client.handleNotification(rpcEnvelope{
		Method: "item/started",
		Params: fileChangeStartedParams(t, "thread", "turn", "item", "/safe/a", "", "first"),
	}, transport, 4)
	params := json.RawMessage(`{"threadId":"thread","turnId":"turn","itemId":"item","startedAtMs":1234}`)
	client.handleServerRequest(rpcEnvelope{ID: json.RawMessage(`51`), Method: "item/fileChange/requestApproval", Params: params}, transport, 4)
	pending := client.PendingRequests()
	if len(pending) != 1 || !pending[0].CanAccept {
		stopPendingRequestTimers(client)
		t.Fatalf("initial request not approvable: %#v", pending)
	}
	replacement, err := parseFileApprovalItemStarted(
		fileChangeStartedParams(t, "thread", "turn", "item", "/safe/b", "", "second"), 4, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	client.putFileApprovalLocked(replacement)
	client.mu.Unlock()
	if err := client.Respond(context.Background(), pending[0].Key, json.RawMessage(`{"decision":"accept"}`)); !errors.Is(err, ErrInvalidServerResponse) {
		stopPendingRequestTimers(client)
		t.Fatalf("replaced evidence accept error = %v", err)
	}
	if err := client.Respond(context.Background(), pending[0].Key, json.RawMessage(`{"decision":"cancel"}`)); err != nil {
		t.Fatalf("cancel after replacement failed: %v", err)
	}

	expired, err := parseFileApprovalItemStarted(
		fileChangeStartedParams(t, "thread", "later", "old", "/safe/a", "", "old"), 4,
		time.Now().Add(-fileApprovalTTL-time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	if client.fileApprovals == nil {
		client.fileApprovals = make(map[fileApprovalKey]*fileApprovalEvidence)
	}
	client.fileApprovals[expired.Key] = expired
	client.fileApprovalOrder = append(client.fileApprovalOrder, expired.Key)
	client.fileApprovalBytes += expired.Bytes
	client.mu.Unlock()
	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`52`), Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread","turnId":"later","itemId":"old","startedAtMs":1234}`),
	}, transport, 4)
	pending = client.PendingRequests()
	if len(pending) != 1 || pending[0].CanAccept {
		stopPendingRequestTimers(client)
		t.Fatalf("expired evidence was accepted: %#v", pending)
	}
	if err := client.Respond(context.Background(), pending[0].Key, json.RawMessage(`{"decision":"decline"}`)); err != nil {
		t.Fatalf("expired request could not be declined: %v", err)
	}
}

func TestV2FileApprovalWorkspaceChangePreventsAccept(t *testing.T) {
	client, transport, _ := newMultiAgentTestClient(11)
	client.AuthorizeThread("thread")
	client.mu.Lock()
	client.putThreadWorkspaceLocked("thread", threadWorkspace{Generation: 11, CWD: "/safe/one"})
	client.mu.Unlock()
	client.handleNotification(rpcEnvelope{
		Method: "item/started",
		Params: fileChangeStartedParams(t, "thread", "turn", "item", "file.go", "", "diff"),
	}, transport, 11)
	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`55`), Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread","turnId":"turn","itemId":"item","startedAtMs":1234}`),
	}, transport, 11)
	pending := client.PendingRequests()
	if len(pending) != 1 || !pending[0].CanAccept {
		stopPendingRequestTimers(client)
		t.Fatalf("initial cwd-correlated request not approvable: %#v", pending)
	}
	client.mu.Lock()
	client.putThreadWorkspaceLocked("thread", threadWorkspace{Generation: 11, CWD: "/safe/two"})
	client.mu.Unlock()
	if err := client.Respond(context.Background(), pending[0].Key, json.RawMessage(`{"decision":"accept"}`)); !errors.Is(err, ErrInvalidServerResponse) {
		stopPendingRequestTimers(client)
		t.Fatalf("changed workspace accept error = %v", err)
	}
	if err := client.Respond(context.Background(), pending[0].Key, json.RawMessage(`{"decision":"decline"}`)); err != nil {
		t.Fatalf("request with changed cwd could not be declined: %v", err)
	}
}

func TestV2FileApprovalRejectsOutsideAndMovePaths(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		movePath string
		root     string
	}{
		{name: "target", path: "/etc/shadow"},
		{name: "move source", path: "/safe/new", movePath: "/etc/shadow"},
		{name: "relative target", path: "relative.txt"},
		{name: "grant root", path: "/safe/new", root: "/etc"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, transport, _ := newMultiAgentTestClient(6)
			client.AuthorizeThread("thread")
			client.handleNotification(rpcEnvelope{
				Method: "item/started",
				Params: fileChangeStartedParams(t, "thread", "turn", "item", test.path, test.movePath, "diff"),
			}, transport, 6)
			params := fmt.Sprintf(`{"threadId":"thread","turnId":"turn","itemId":"item","startedAtMs":1234,"grantRoot":%q}`, test.root)
			if test.root == "" {
				params = `{"threadId":"thread","turnId":"turn","itemId":"item","startedAtMs":1234}`
			}
			client.handleServerRequest(rpcEnvelope{
				ID: json.RawMessage(fmt.Sprintf("%d", 60+index)), Method: "item/fileChange/requestApproval", Params: json.RawMessage(params),
			}, transport, 6)
			pending := client.PendingRequests()
			if len(pending) != 1 || pending[0].CanAccept {
				stopPendingRequestTimers(client)
				t.Fatalf("unsafe path marked approvable: %#v", pending)
			}
			if err := client.Respond(context.Background(), pending[0].Key, json.RawMessage(`{"decision":"decline"}`)); err != nil {
				t.Fatalf("unsafe request could not be declined: %v", err)
			}
		})
	}
}

func TestFileApprovalCacheIsBoundedAndCleanedByLifecycle(t *testing.T) {
	client, transport, _ := newMultiAgentTestClient(8)
	client.AuthorizeThread("thread")
	for index := 0; index < maxFileApprovalEntries+8; index++ {
		client.handleNotification(rpcEnvelope{
			Method: "item/started",
			Params: fileChangeStartedParams(t, "thread", "turn", fmt.Sprintf("item-%d", index), "/safe/a", "", "diff"),
		}, transport, 8)
	}
	client.mu.Lock()
	entries, order, bytes := len(client.fileApprovals), len(client.fileApprovalOrder), client.fileApprovalBytes
	client.mu.Unlock()
	if entries > maxFileApprovalEntries || order != entries || bytes > maxFileApprovalBytes {
		t.Fatalf("cache limits exceeded: entries=%d order=%d bytes=%d", entries, order, bytes)
	}
	client.handleNotification(rpcEnvelope{
		Method: "turn/completed", Params: json.RawMessage(`{"threadId":"thread","turn":{"id":"turn","items":[]}}`),
	}, transport, 8)
	client.mu.Lock()
	entries, bytes = len(client.fileApprovals), client.fileApprovalBytes
	client.mu.Unlock()
	if entries != 0 || bytes != 0 {
		t.Fatalf("turn completion retained evidence: entries=%d bytes=%d", entries, bytes)
	}

	client.handleNotification(rpcEnvelope{
		Method: "item/started",
		Params: fileChangeStartedParams(t, "thread", "next", "item", "/safe/a", "", "diff"),
	}, transport, 8)
	client.handleNotification(rpcEnvelope{
		Method: "item/completed", Params: json.RawMessage(`{"threadId":"thread","turnId":"next","item":{"id":"item"}}`),
	}, transport, 8)
	client.mu.Lock()
	entries = len(client.fileApprovals)
	client.mu.Unlock()
	if entries != 0 {
		t.Fatalf("item completion retained %d evidence entries", entries)
	}

	client.handleNotification(rpcEnvelope{
		Method: "item/started",
		Params: fileChangeStartedParams(t, "thread", "last", "item", "/safe/a", "", "diff"),
	}, transport, 8)
	client.disconnect(transport, 8, "test cleanup")
	client.mu.Lock()
	entries, order, bytes = len(client.fileApprovals), len(client.fileApprovalOrder), client.fileApprovalBytes
	client.mu.Unlock()
	if entries != 0 || order != 0 || bytes != 0 {
		t.Fatalf("disconnect retained cache: entries=%d order=%d bytes=%d", entries, order, bytes)
	}
}

func TestThreadWorkspaceCaptureFiltersByPathAndIsBounded(t *testing.T) {
	client, _, _ := newMultiAgentTestClient(12)
	client.captureThreadWorkspaces("thread/list", json.RawMessage(`{
		"data":[
			{"id":"inside","cwd":"/safe/work"},
			{"id":"outside","cwd":"/etc"},
			{"id":"relative","cwd":"work"}
		]
	}`), 12)
	client.mu.Lock()
	_, inside := client.threadWorkspaces["inside"]
	_, outside := client.threadWorkspaces["outside"]
	_, relative := client.threadWorkspaces["relative"]
	client.mu.Unlock()
	if !inside || outside || relative {
		t.Fatalf("thread workspace filtering failed: inside=%t outside=%t relative=%t", inside, outside, relative)
	}

	client.mu.Lock()
	for index := 0; index < maxThreadWorkspaces+10; index++ {
		client.putThreadWorkspaceLocked(fmt.Sprintf("thread-%d", index), threadWorkspace{Generation: 12, CWD: "/safe/work"})
	}
	entries, order := len(client.threadWorkspaces), len(client.threadWorkspaceOrder)
	client.mu.Unlock()
	if entries > maxThreadWorkspaces || entries != order {
		t.Fatalf("thread workspace cache exceeded bounds: entries=%d order=%d", entries, order)
	}
}

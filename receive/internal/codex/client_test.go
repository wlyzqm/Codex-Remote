package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryTransport struct {
	mu     sync.Mutex
	writes [][]byte
	closed bool
}

func (m *memoryTransport) ReadMessage() ([]byte, error) { return nil, io.EOF }
func (m *memoryTransport) WriteMessage(message []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return io.ErrClosedPipe
	}
	m.writes = append(m.writes, cloneRaw(message))
	return nil
}
func (m *memoryTransport) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}
func (m *memoryTransport) Kind() string { return "memory" }

func (m *memoryTransport) snapshotWrites() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	writes := make([][]byte, len(m.writes))
	for index, write := range m.writes {
		writes[index] = cloneRaw(write)
	}
	return writes
}

type eventRecorder struct {
	mu     sync.Mutex
	events []map[string]any
}

func (r *eventRecorder) emit(value any) {
	event, ok := value.(map[string]any)
	if !ok {
		return
	}
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *eventRecorder) notificationMethods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var methods []string
	for _, event := range r.events {
		if event["type"] != "notification" {
			continue
		}
		if method, ok := event["method"].(string); ok {
			methods = append(methods, method)
		}
	}
	return methods
}

func (r *eventRecorder) countType(wanted string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, event := range r.events {
		if event["type"] == wanted {
			count++
		}
	}
	return count
}

func newMultiAgentTestClient(generation uint64) (*Client, *memoryTransport, *eventRecorder) {
	transport := &memoryTransport{}
	recorder := &eventRecorder{}
	client := &Client{
		cfg: Config{
			Emit: recorder.emit, CheckPath: safeTestPath,
			Logger: log.New(io.Discard, "", 0),
		},
		current:           transport,
		ready:             true,
		generation:        generation,
		pending:           make(map[int64]chan rpcReply),
		requests:          make(map[string]pendingServerRequest),
		activeTurns:       make(map[string]struct{}),
		authorizedThreads: make(map[string]struct{}),
		childCandidates:   make(map[string]*childCandidate),
	}
	return client, transport, recorder
}

func waitForCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}

func waitForRPCMethod(t *testing.T, transport *memoryTransport, method string) rpcEnvelope {
	t.Helper()
	var found rpcEnvelope
	waitForCondition(t, func() bool {
		for _, write := range transport.snapshotWrites() {
			var envelope rpcEnvelope
			if json.Unmarshal(write, &envelope) == nil && envelope.Method == method {
				found = envelope
				return true
			}
		}
		return false
	})
	return found
}

func validChildReadResult(parentID, childID, agentPath, cwd string) json.RawMessage {
	value, _ := json.Marshal(map[string]any{
		"thread": map[string]any{
			"id": childID, "parentThreadId": parentID, "cwd": cwd,
			"source": map[string]any{
				"subAgent": map[string]any{
					"thread_spawn": map[string]any{
						"parent_thread_id": parentID, "depth": 1, "agent_path": agentPath,
					},
				},
			},
		},
	})
	return value
}

func validChildStartedParams(parentID, childID, agentPath, cwd string) json.RawMessage {
	result := validChildReadResult(parentID, childID, agentPath, cwd)
	var value map[string]json.RawMessage
	_ = json.Unmarshal(result, &value)
	return json.RawMessage(`{"thread":` + string(value["thread"]) + `}`)
}

func TestResolvedServerRequestEmitsOpaqueKey(t *testing.T) {
	var emitted []map[string]any
	client := &Client{
		cfg: Config{Emit: func(value any) {
			event, ok := value.(map[string]any)
			if !ok {
				t.Fatalf("unexpected event type %T", value)
			}
			emitted = append(emitted, event)
		}},
		requests: map[string]pendingServerRequest{
			"opaque-browser-key": {ID: json.RawMessage(`42`)},
		},
		activeTurns: make(map[string]struct{}), authorizedThreads: map[string]struct{}{"thread-1": {}},
	}
	transport := &memoryTransport{}
	client.current = transport
	client.generation = 7
	client.handleNotification(rpcEnvelope{
		Method: "serverRequest/resolved",
		Params: json.RawMessage(`{"threadId":"thread-1","requestId":42}`),
	}, transport, 7)
	if len(client.requests) != 0 {
		t.Fatal("resolved request remained pending")
	}
	if client.requestBytes != 0 {
		t.Fatalf("resolved request retained %d bytes", client.requestBytes)
	}
	if len(emitted) < 1 || emitted[0]["type"] != "server_request_answered" || emitted[0]["key"] != "opaque-browser-key" {
		t.Fatalf("opaque resolution event missing: %#v", emitted)
	}
}

func TestReadLineLimited(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader("1234567\n"), 4)
	line, err := readLineLimited(reader, 8)
	if err != nil || string(line) != "1234567\n" {
		t.Fatalf("valid line = %q, %v", line, err)
	}
	reader = bufio.NewReaderSize(strings.NewReader("12345678\n"), 4)
	if _, err := readLineLimited(reader, 8); err == nil {
		t.Fatal("oversized app-server line was accepted")
	}
}

func TestEnsureConnectedWaitsUntilInitializationIsReady(t *testing.T) {
	transport := &memoryTransport{}
	client := &Client{
		cfg:         Config{Mode: "daemon"},
		current:     transport,
		generation:  1,
		pending:     make(map[int64]chan rpcReply),
		requests:    make(map[string]pendingServerRequest),
		activeTurns: make(map[string]struct{}),
	}
	client.startMu.Lock()
	done := make(chan error, 1)
	go func() { done <- client.ensureConnected(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("connection escaped before initialization completed: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	client.mu.Lock()
	client.ready = true
	client.mu.Unlock()
	client.startMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ready connection rejected: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("connection did not unblock after initialization")
	}
}

func TestOldGenerationCannotPublishServerRequest(t *testing.T) {
	oldTransport := &memoryTransport{}
	newTransport := &memoryTransport{}
	client := &Client{
		cfg:         Config{Emit: func(any) { t.Fatal("stale request emitted an event") }},
		current:     newTransport,
		ready:       true,
		generation:  2,
		requests:    make(map[string]pendingServerRequest),
		activeTurns: make(map[string]struct{}),
	}
	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`9`), Method: "item/fileChange/requestApproval", Params: json.RawMessage(`{}`),
	}, oldTransport, 1)
	if len(client.requests) != 0 {
		t.Fatal("stale connection inserted a request into the new generation")
	}
}

func TestRespondKeepsRequestAfterInvalidDecision(t *testing.T) {
	transport := &memoryTransport{}
	params := json.RawMessage(`{"command":"cat file","cwd":"/safe","commandActions":[{"type":"read","path":"/safe/file"}],"availableDecisions":["accept","acceptForSession","decline"]}`)
	request := pendingServerRequest{
		PublicRequest: PublicRequest{Key: "opaque", Method: "item/commandExecution/requestApproval", Params: params},
		ID:            json.RawMessage(`17`), Generation: 1,
	}
	client := &Client{
		cfg: Config{Emit: func(any) {}, CheckPath: safeTestPath}, current: transport, ready: true, generation: 1,
		requests: map[string]pendingServerRequest{"opaque": request}, requestBytes: len(params),
		activeTurns: make(map[string]struct{}),
	}
	if err := client.Respond(context.Background(), "opaque", json.RawMessage(`{"decision":"acceptForSession"}`)); !errors.Is(err, ErrInvalidServerResponse) {
		t.Fatalf("unsafe decision error = %v", err)
	}
	if len(client.requests) != 1 {
		t.Fatal("invalid response consumed the pending request")
	}
	if err := client.Respond(context.Background(), "opaque", json.RawMessage(`{"decision":"accept"}`)); err != nil {
		t.Fatalf("one-shot decision failed: %v", err)
	}
	if len(client.requests) != 0 || len(transport.writes) != 1 || !strings.Contains(string(transport.writes[0]), `"decision":"accept"`) {
		t.Fatalf("approval was not written exactly once: requests=%d writes=%q", len(client.requests), transport.writes)
	}
}

func TestClientConfigSeparatesWorkspaceAndTargetChecks(t *testing.T) {
	client, transport, _ := newMultiAgentTestClient(1)
	client.cfg.CheckWorkspace = func(path string) error {
		if path == "/root" {
			return nil
		}
		return errors.New("workspace rejected")
	}
	client.cfg.CheckPath = func(path string) error {
		if path == "/root/project/input" {
			return nil
		}
		return errors.New("protected target")
	}
	client.AuthorizeThread("thread")

	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`101`), Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread","command":"cat input","cwd":"/root","commandActions":[{"type":"read","path":"project/input"}]}`),
	}, transport, 1)
	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`102`), Method: "item/commandExecution/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread","command":"cat config","cwd":"/root","commandActions":[{"type":"read","path":".codex/config.toml"}]}`),
	}, transport, 1)

	requests := client.PendingRequests()
	stopPendingRequestTimers(client)
	if len(requests) != 2 {
		t.Fatalf("pending requests = %#v", requests)
	}
	var allowed, protected bool
	for _, request := range requests {
		switch {
		case strings.Contains(string(request.Params), "project/input"):
			allowed = request.CanAccept
		case strings.Contains(string(request.Params), ".codex/config.toml"):
			protected = !request.CanAccept
		}
	}
	if !allowed || !protected {
		t.Fatalf("split checker decisions were not preserved: %#v", requests)
	}
}

func TestServerRequestRequiresAuthorizedThread(t *testing.T) {
	transport := &memoryTransport{}
	emitted := 0
	client := &Client{
		cfg: Config{Emit: func(any) { emitted++ }}, current: transport, ready: true, generation: 3,
		requests: make(map[string]pendingServerRequest), activeTurns: make(map[string]struct{}),
		authorizedThreads: make(map[string]struct{}),
	}
	envelope := rpcEnvelope{
		ID: json.RawMessage(`21`), Method: "item/fileChange/requestApproval",
		Params: json.RawMessage(`{"threadId":"thread-safe","turnId":"turn-1","itemId":"item-1"}`),
	}
	client.handleServerRequest(envelope, transport, 3)
	if len(client.requests) != 0 || len(transport.writes) != 1 || emitted != 0 {
		t.Fatalf("unauthorized request leaked: pending=%d writes=%d events=%d", len(client.requests), len(transport.writes), emitted)
	}
	client.AuthorizeThread("thread-safe")
	client.handleServerRequest(envelope, transport, 3)
	if len(client.requests) != 1 || emitted != 1 {
		t.Fatalf("authorized request was not exposed: pending=%d events=%d", len(client.requests), emitted)
	}
	for _, request := range client.requests {
		request.Timer.Stop()
	}
}

func TestAuthorizedThreadOutsideWorkspaceIsRevoked(t *testing.T) {
	client, transport, recorder := newMultiAgentTestClient(12)
	client.AuthorizeThread("thread-safe")
	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`81`), Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"threadId":"thread-safe","turnId":"turn","itemId":"question","questions":[{"id":"answer"}]}`),
	}, transport, 12)
	if len(client.requests) != 1 {
		t.Fatal("authorized request was not pending before cwd drift")
	}

	client.handleNotification(rpcEnvelope{
		Method: "thread/started",
		Params: json.RawMessage(`{"thread":{"id":"thread-safe","cwd":"/outside","source":"cli"}}`),
	}, transport, 12)

	client.mu.Lock()
	_, authorized := client.authorizedThreads["thread-safe"]
	pending := len(client.requests)
	client.mu.Unlock()
	if authorized || pending != 0 {
		t.Fatalf("outside-workspace thread retained authority: authorized=%v pending=%d", authorized, pending)
	}
	if methods := recorder.notificationMethods(); len(methods) != 0 {
		t.Fatalf("outside-workspace thread notification leaked: %#v", methods)
	}
	writes := transport.snapshotWrites()
	if len(writes) != 1 || !strings.Contains(string(writes[0]), `thread working directory is no longer authorized`) {
		t.Fatalf("pending request was not rejected on cwd drift: %q", writes)
	}

	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`82`), Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"threadId":"thread-safe","turnId":"turn","itemId":"later","questions":[{"id":"answer"}]}`),
	}, transport, 12)
	if len(client.requests) != 0 || len(transport.snapshotWrites()) != 2 {
		t.Fatal("revoked thread regained remote request authority")
	}
}

func TestDisconnectClearsThreadAuthority(t *testing.T) {
	client, transport, _ := newMultiAgentTestClient(13)
	client.AuthorizeThread("thread-safe")
	client.disconnectLocked(transport, 13, "test disconnect")
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.authorizedThreads) != 0 || len(client.authorizedOrder) != 0 {
		t.Fatalf("thread authority survived transport generation: %#v", client.authorizedThreads)
	}
}

func TestNotificationIDsReadsNestedThreadID(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		threadID string
		turnID   string
	}{
		{
			name: "thread started", raw: `{"thread":{"id":"nested-thread"}}`,
			threadID: "nested-thread",
		},
		{
			name: "nested turn", raw: `{"conversationId":"conversation","turn":{"id":"nested-turn"}}`,
			threadID: "conversation", turnID: "nested-turn",
		},
		{
			name:     "top level fields take precedence",
			raw:      `{"threadId":"top-thread","conversationId":"conversation","thread":{"id":"nested-thread"},"turnId":"top-turn","turn":{"id":"nested-turn"}}`,
			threadID: "top-thread", turnID: "top-turn",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			threadID, turnID := notificationIDs(json.RawMessage(test.raw))
			if threadID != test.threadID || turnID != test.turnID {
				t.Fatalf("notification IDs = %q/%q, want %q/%q", threadID, turnID, test.threadID, test.turnID)
			}
		})
	}
}

func TestChildSeedExtractionIsNarrowAndParentBound(t *testing.T) {
	tests := []struct {
		name     string
		item     string
		expected map[string]string
	}{
		{
			name:     "subagent activity",
			item:     `{"type":"subAgentActivity","agentThreadId":"child-a","agentPath":"/root/a","untrustedThreadId":"stranger"}`,
			expected: map[string]string{"child-a": "/root/a"},
		},
		{
			name:     "current collab tool",
			item:     `{"type":"collabAgentToolCall","senderThreadId":"parent","receiverThreadIds":["child-a","child-b","child-a"],"agentsStates":{"child-c":{"status":"running"},"child-b":{"status":"completed"}}}`,
			expected: map[string]string{"child-a": "", "child-b": "", "child-c": ""},
		},
		{
			name:     "legacy collab tool",
			item:     `{"type":"collabToolCall","senderThreadId":"parent","receiverThreadId":"child-a","newThreadId":"child-b","agentThreadId":"child-c","receiverThreadIds":["child-d"],"agentsStates":{"child-e":{}}}`,
			expected: map[string]string{"child-a": "", "child-b": "", "child-c": "", "child-d": "", "child-e": ""},
		},
		{
			name:     "different collab sender",
			item:     `{"type":"collabAgentToolCall","senderThreadId":"other-parent","receiverThreadIds":["stranger"]}`,
			expected: map[string]string{},
		},
		{
			name:     "unrelated item strings",
			item:     `{"type":"agentMessage","threadId":"stranger","receiverThreadIds":["stranger-two"]}`,
			expected: map[string]string{},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			seeds := childSeedsFromItem("parent", json.RawMessage(test.item))
			actual := make(map[string]string, len(seeds))
			for _, seed := range seeds {
				if seed.ParentID != "parent" {
					t.Fatalf("seed parent = %q", seed.ParentID)
				}
				actual[seed.ChildID] = seed.AgentPath
			}
			if len(actual) != len(test.expected) {
				t.Fatalf("seeds = %#v, want %#v", actual, test.expected)
			}
			for childID, path := range test.expected {
				if actual[childID] != path {
					t.Fatalf("seed %q path = %q, want %q (all: %#v)", childID, actual[childID], path, actual)
				}
			}
		})
	}

	turn := json.RawMessage(`{"threadId":"parent","turn":{"id":"turn","items":[{"type":"subAgentActivity","agentThreadId":"child-a","agentPath":"/root/a"},{"type":"subAgentActivity","agentThreadId":"child-a","agentPath":"/root/a"}]}}`)
	if seeds := childSeedsFromNotification("turn/completed", "parent", turn); len(seeds) != 1 || seeds[0].ChildID != "child-a" {
		t.Fatalf("turn item seeds were not deduplicated: %#v", seeds)
	}
}

func TestValidateChildThreadResultRequiresDirectSpawnAndSafeCWD(t *testing.T) {
	seed := childCandidateSeed{ParentID: "parent", ChildID: "child", AgentPath: "/root/child"}
	valid := validChildReadResult("parent", "child", "/root/child", "/safe/work")
	if err := validateChildThreadResult(valid, seed, safeTestPath); err != nil {
		t.Fatalf("valid direct child was rejected: %v", err)
	}

	tests := []struct {
		name      string
		result    json.RawMessage
		checkPath func(string) error
	}{
		{name: "missing path checker", result: valid, checkPath: nil},
		{name: "different child", result: validChildReadResult("parent", "other", "/root/child", "/safe/work"), checkPath: safeTestPath},
		{name: "different parent", result: validChildReadResult("other-parent", "child", "/root/child", "/safe/work"), checkPath: safeTestPath},
		{name: "different agent path", result: validChildReadResult("parent", "child", "/root/other", "/safe/work"), checkPath: safeTestPath},
		{name: "unsafe cwd", result: validChildReadResult("parent", "child", "/root/child", "/outside"), checkPath: safeTestPath},
		{
			name:      "source parent contradicts thread parent",
			result:    json.RawMessage(`{"thread":{"id":"child","parentThreadId":"parent","cwd":"/safe/work","source":{"subAgent":{"thread_spawn":{"parent_thread_id":"other-parent","agent_path":"/root/child"}}}}}`),
			checkPath: safeTestPath,
		},
		{
			name:      "non-spawn subagent source",
			result:    json.RawMessage(`{"thread":{"id":"child","parentThreadId":"parent","cwd":"/safe/work","source":{"subAgent":"review"}}}`),
			checkPath: safeTestPath,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateChildThreadResult(test.result, seed, test.checkPath); err == nil {
				t.Fatal("unverified child relationship was accepted")
			}
		})
	}
}

func TestVerifiedChildNotificationsAndServerRequestsAreForwarded(t *testing.T) {
	client, transport, recorder := newMultiAgentTestClient(5)
	client.AuthorizeThread("parent")
	client.handleNotification(rpcEnvelope{
		Method: "item/started",
		Params: json.RawMessage(`{"threadId":"parent","turnId":"parent-turn","item":{"type":"subAgentActivity","id":"activity","kind":"started","agentThreadId":"child","agentPath":"/root/child"}}`),
	}, transport, 5)
	readRequest := waitForRPCMethod(t, transport, "thread/read")

	client.handleNotification(rpcEnvelope{
		Method: "thread/started", Params: validChildStartedParams("parent", "child", "/root/child", "/safe/work"),
	}, transport, 5)
	client.handleNotification(rpcEnvelope{
		Method: "thread/status/changed", Params: json.RawMessage(`{"threadId":"child","status":{"type":"active"}}`),
	}, transport, 5)
	client.handleNotification(rpcEnvelope{
		Method: "turn/started", Params: json.RawMessage(`{"threadId":"child","turn":{"id":"child-turn","items":[]}}`),
	}, transport, 5)
	client.handleNotification(rpcEnvelope{
		Method: "item/started", Params: json.RawMessage(`{"threadId":"child","turnId":"child-turn","item":{"type":"agentMessage","id":"message","text":"hello"}}`),
	}, transport, 5)
	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`44`), Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"threadId":"child","turnId":"child-turn","itemId":"question","questions":[{"id":"answer","header":"Answer","question":"Continue?"}]}`),
	}, transport, 5)

	if methods := recorder.notificationMethods(); len(methods) != 1 || methods[0] != "item/started" {
		t.Fatalf("unverified child notifications leaked: %#v", methods)
	}
	client.mu.Lock()
	pendingBeforeVerification := len(client.requests)
	queuedBeforeVerification := client.childEventCount
	client.mu.Unlock()
	if pendingBeforeVerification != 0 || queuedBeforeVerification != 5 {
		t.Fatalf("pre-verification state: pending=%d queued=%d", pendingBeforeVerification, queuedBeforeVerification)
	}

	client.handleReply(rpcEnvelope{ID: readRequest.ID, Result: validChildReadResult("parent", "child", "/root/child", "/safe/work")})
	waitForCondition(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		_, authorized := client.authorizedThreads["child"]
		return authorized && len(client.requests) == 1 && len(client.activeTurns) == 1 && client.childEventCount == 0
	})

	wantMethods := []string{"item/started", "thread/started", "thread/status/changed", "turn/started", "item/started"}
	if methods := recorder.notificationMethods(); strings.Join(methods, ",") != strings.Join(wantMethods, ",") {
		t.Fatalf("forwarded notification order = %#v, want %#v", methods, wantMethods)
	}
	if recorder.countType("server_request") != 1 {
		t.Fatalf("child server request event count = %d", recorder.countType("server_request"))
	}
	client.mu.Lock()
	if client.childCandidates["child"] != nil || client.childEventBytes != 0 {
		client.mu.Unlock()
		t.Fatal("verified child candidate retained buffered state")
	}
	for key, request := range client.requests {
		if request.Method != "item/tool/requestUserInput" {
			client.mu.Unlock()
			t.Fatalf("pending child request method = %q", request.Method)
		}
		if request.Timer != nil {
			request.Timer.Stop()
		}
		delete(client.requests, key)
	}
	client.mu.Unlock()
}

func TestFailedChildVerificationDropsNotificationsAndRejectsRequests(t *testing.T) {
	client, transport, recorder := newMultiAgentTestClient(6)
	client.AuthorizeThread("parent")
	client.handleNotification(rpcEnvelope{
		Method: "item/completed",
		Params: json.RawMessage(`{"threadId":"parent","turnId":"parent-turn","item":{"type":"collabAgentToolCall","id":"collab","senderThreadId":"parent","receiverThreadIds":["child"],"agentsStates":{"child":{"status":"running"}}}}`),
	}, transport, 6)
	readRequest := waitForRPCMethod(t, transport, "thread/read")
	client.handleNotification(rpcEnvelope{
		Method: "thread/status/changed", Params: json.RawMessage(`{"threadId":"child","status":{"type":"active"}}`),
	}, transport, 6)
	client.handleServerRequest(rpcEnvelope{
		ID: json.RawMessage(`73`), Method: "item/tool/requestUserInput",
		Params: json.RawMessage(`{"threadId":"child","turnId":"turn","itemId":"question","questions":[{"id":"answer"}]}`),
	}, transport, 6)

	client.handleReply(rpcEnvelope{ID: readRequest.ID, Result: validChildReadResult("different-parent", "child", "", "/safe/work")})
	waitForCondition(t, func() bool {
		client.mu.Lock()
		candidateGone := client.childCandidates["child"] == nil
		_, authorized := client.authorizedThreads["child"]
		pending := len(client.requests)
		client.mu.Unlock()
		return candidateGone && !authorized && pending == 0 && len(transport.snapshotWrites()) >= 2
	})
	if methods := recorder.notificationMethods(); len(methods) != 1 || methods[0] != "item/completed" {
		t.Fatalf("failed child notification leaked: %#v", methods)
	}
	writes := transport.snapshotWrites()
	var rejection rpcEnvelope
	if err := json.Unmarshal(writes[len(writes)-1], &rejection); err != nil || rawIDString(rejection.ID) != "73" {
		t.Fatalf("buffered server request was not rejected: %q", writes[len(writes)-1])
	}
	var rpcErr RPCError
	if json.Unmarshal(rejection.Error, &rpcErr) != nil || rpcErr.Code != -32001 {
		t.Fatalf("child rejection = %s", rejection.Error)
	}
}

func TestThreadStartedRequiresAuthorizedVerifiedParentRelationship(t *testing.T) {
	t.Run("unknown parent is dropped", func(t *testing.T) {
		client, transport, recorder := newMultiAgentTestClient(7)
		client.handleNotification(rpcEnvelope{
			Method: "thread/started", Params: validChildStartedParams("unknown-parent", "stranger", "/root/stranger", "/safe/work"),
		}, transport, 7)
		time.Sleep(20 * time.Millisecond)
		client.mu.Lock()
		_, authorized := client.authorizedThreads["stranger"]
		candidateCount := len(client.childCandidates)
		client.mu.Unlock()
		if authorized || candidateCount != 0 || len(transport.snapshotWrites()) != 0 || len(recorder.notificationMethods()) != 0 {
			t.Fatal("unknown thread/started created authority or observable state")
		}
	})

	t.Run("non-spawn source is dropped", func(t *testing.T) {
		client, transport, recorder := newMultiAgentTestClient(8)
		client.AuthorizeThread("parent")
		client.handleNotification(rpcEnvelope{
			Method: "thread/started",
			Params: json.RawMessage(`{"thread":{"id":"stranger","parentThreadId":"parent","cwd":"/safe/work","source":{"subAgent":"review"}}}`),
		}, transport, 8)
		time.Sleep(20 * time.Millisecond)
		client.mu.Lock()
		_, authorized := client.authorizedThreads["stranger"]
		candidateCount := len(client.childCandidates)
		client.mu.Unlock()
		if authorized || candidateCount != 0 || len(transport.snapshotWrites()) != 0 || len(recorder.notificationMethods()) != 0 {
			t.Fatal("non-spawn thread/started created authority or observable state")
		}
	})

	t.Run("direct child is buffered until thread read", func(t *testing.T) {
		client, transport, recorder := newMultiAgentTestClient(9)
		client.AuthorizeThread("parent")
		client.handleNotification(rpcEnvelope{
			Method: "thread/started", Params: validChildStartedParams("parent", "child", "/root/child", "/safe/work"),
		}, transport, 9)
		readRequest := waitForRPCMethod(t, transport, "thread/read")
		if len(recorder.notificationMethods()) != 0 {
			t.Fatal("thread/started leaked before verification")
		}
		client.handleReply(rpcEnvelope{ID: readRequest.ID, Result: validChildReadResult("parent", "child", "/root/child", "/safe/work")})
		waitForCondition(t, func() bool {
			client.mu.Lock()
			_, authorized := client.authorizedThreads["child"]
			client.mu.Unlock()
			return authorized && len(recorder.notificationMethods()) == 1
		})
		if methods := recorder.notificationMethods(); len(methods) != 1 || methods[0] != "thread/started" {
			t.Fatalf("verified thread/started was not forwarded: %#v", methods)
		}
	})
}

func TestChildCandidateBacklogIsBounded(t *testing.T) {
	client, transport, _ := newMultiAgentTestClient(10)
	client.AuthorizeThread("parent")
	client.mu.Lock()
	candidate, created := client.addChildCandidateLocked(childCandidateSeed{ParentID: "parent", ChildID: "child"}, 10)
	if candidate == nil || !created {
		client.mu.Unlock()
		t.Fatal("could not create bounded-queue candidate")
	}
	for index := 0; index < maxChildCandidateEvents; index++ {
		if !client.queueChildMessageLocked(candidate, rpcEnvelope{Method: "thread/status/changed", Params: json.RawMessage(`{"threadId":"child"}`)}, false) {
			client.mu.Unlock()
			t.Fatalf("queue rejected event %d before the count bound", index)
		}
	}
	if client.queueChildMessageLocked(candidate, rpcEnvelope{Method: "thread/status/changed", Params: json.RawMessage(`{"threadId":"child"}`)}, false) {
		client.mu.Unlock()
		t.Fatal("queue accepted an event beyond the count bound")
	}
	client.mu.Unlock()
	if len(transport.snapshotWrites()) != 0 {
		t.Fatal("bounded queue unexpectedly wrote to app-server")
	}

	client, _, _ = newMultiAgentTestClient(11)
	client.AuthorizeThread("parent")
	client.mu.Lock()
	candidate, _ = client.addChildCandidateLocked(childCandidateSeed{ParentID: "parent", ChildID: "child"}, 11)
	oversized := rpcEnvelope{Method: "item/started", Params: json.RawMessage(strings.Repeat("x", maxChildCandidateBytes+1))}
	if client.queueChildMessageLocked(candidate, oversized, false) {
		client.mu.Unlock()
		t.Fatal("queue accepted an event beyond the byte bound")
	}
	client.mu.Unlock()
}

package codex

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"sync"
	"time"
)

type Config struct {
	Mode           string
	CodexBin       string
	CodexHome      string
	DaemonSocket   string
	IdleTimeout    time.Duration
	Logger         *log.Logger
	Emit           func(any)
	CheckWorkspace func(string) error
	CheckPath      func(string) error
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("app-server error %d: %s", e.Code, e.Message)
}

type rpcEnvelope struct {
	ID          json.RawMessage `json:"id,omitempty"`
	Method      string          `json:"method,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
	EmittedAtMS *int64          `json:"emittedAtMs,omitempty"`
}

type rpcReply struct {
	Result json.RawMessage
	Error  *RPCError
	Cause  error
}

type PublicRequest struct {
	Key          string          `json:"key"`
	Method       string          `json:"method"`
	Params       json.RawMessage `json:"params"`
	FileChanges  json.RawMessage `json:"fileChanges,omitempty"`
	CanAccept    bool            `json:"canAccept"`
	PolicyReason string          `json:"policyReason,omitempty"`
}

type pendingServerRequest struct {
	PublicRequest
	ID           json.RawMessage
	Generation   uint64
	Timer        *time.Timer
	FileApproval *fileApprovalEvidence
}

type Status struct {
	Connected       bool   `json:"connected"`
	Transport       string `json:"transport"`
	Generation      uint64 `json:"generation"`
	ActiveTurns     int    `json:"activeTurns"`
	PendingRequests int    `json:"pendingRequests"`
	PendingReqBytes int    `json:"pendingRequestBytes"`
	PendingRPCs     int    `json:"pendingRpcs"`
	CodexBinary     string `json:"codexBinary,omitempty"`
	NativeBinary    bool   `json:"nativeBinary"`
	IdleTimeoutSec  int64  `json:"idleTimeoutSec"`
}

const (
	maxPendingServerRequests = 64
	maxPendingRequestBytes   = 2 << 20
	maxAuthorizedThreads     = 2048
	maxChildCandidates       = 64
	maxChildCandidateEvents  = 256
	maxChildCandidateBytes   = 1 << 20
)

type childCandidateSeed struct {
	ParentID  string
	ChildID   string
	AgentPath string
}

type queuedChildMessage struct {
	Envelope      rpcEnvelope
	ServerRequest bool
	Bytes         int
}

type childCandidate struct {
	childCandidateSeed
	Generation uint64
	Queue      []queuedChildMessage
}

// Client owns one connection to Codex app-server. In auto mode it first reuses
// Codex's managed Unix-socket daemon; if no daemon is reachable, it lazily
// starts the native Rust binary and tears it down after an idle window.
type Client struct {
	cfg Config

	startMu              sync.Mutex
	mu                   sync.Mutex
	closed               bool
	current              messageTransport
	ready                bool
	generation           uint64
	nextID               int64
	pending              map[int64]chan rpcReply
	requests             map[string]pendingServerRequest
	requestBytes         int
	activeTurns          map[string]struct{}
	authorizedThreads    map[string]struct{}
	authorizedOrder      []string
	childCandidates      map[string]*childCandidate
	childEventCount      int
	childEventBytes      int
	fileApprovals        map[fileApprovalKey]*fileApprovalEvidence
	fileApprovalOrder    []fileApprovalKey
	fileApprovalBytes    int
	threadWorkspaces     map[string]threadWorkspace
	threadWorkspaceOrder []string
	lastActivity         time.Time
	transportKind        string
	resolved             resolvedBinary
	idleStop             chan struct{}
}

func New(cfg Config) (*Client, error) {
	if cfg.Mode == "" {
		cfg.Mode = "auto"
	}
	if cfg.Mode != "auto" && cfg.Mode != "daemon" && cfg.Mode != "spawn" {
		return nil, fmt.Errorf("invalid app-server mode %q", cfg.Mode)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.New(os.Stderr, "codex-remote: ", log.LstdFlags)
	}
	if cfg.Emit == nil {
		cfg.Emit = func(any) {}
	}
	if cfg.DaemonSocket == "" {
		path, err := daemonSocketPath(cfg.CodexHome)
		if err != nil {
			return nil, err
		}
		cfg.DaemonSocket = path
	}
	c := &Client{
		cfg:               cfg,
		pending:           make(map[int64]chan rpcReply),
		requests:          make(map[string]pendingServerRequest),
		activeTurns:       make(map[string]struct{}),
		authorizedThreads: make(map[string]struct{}),
		childCandidates:   make(map[string]*childCandidate),
		fileApprovals:     make(map[fileApprovalKey]*fileApprovalEvidence),
		threadWorkspaces:  make(map[string]threadWorkspace),
		lastActivity:      time.Now(),
		idleStop:          make(chan struct{}),
	}
	if cfg.IdleTimeout > 0 {
		go c.idleLoop()
	}
	return c, nil
}

func (c *Client) workspaceChecker() func(string) error {
	if c.cfg.CheckWorkspace != nil {
		return c.cfg.CheckWorkspace
	}
	return c.cfg.CheckPath
}

func (c *Client) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	if err := c.ensureConnected(ctx); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	transport := c.current
	generation := c.generation
	c.mu.Unlock()
	result, rpcErr, err := c.callOn(ctx, transport, generation, method, params)
	if err == nil && rpcErr == nil {
		c.captureThreadWorkspaces(method, result, generation)
	}
	return result, rpcErr, err
}

func (c *Client) ensureConnected(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errTransportClosed
	}
	if c.current != nil && c.ready {
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errTransportClosed
	}
	if c.current != nil && c.ready {
		c.mu.Unlock()
		return nil
	}
	if c.current != nil {
		// A previous activation cannot still be initializing here because it
		// holds startMu. Treat this as a failed half-open connection.
		transport := c.current
		generation := c.generation
		c.mu.Unlock()
		c.disconnectLocked(transport, generation, "backend was not ready")
	} else {
		c.mu.Unlock()
	}

	var failures []error
	if c.cfg.Mode == "auto" || c.cfg.Mode == "daemon" {
		if info, err := os.Stat(c.cfg.DaemonSocket); err == nil && info.Mode()&os.ModeSocket != 0 {
			transport, err := dialUnixWebSocket(c.cfg.DaemonSocket, 4*time.Second)
			if err == nil {
				if err = c.activate(ctx, transport, resolvedBinary{}); err == nil {
					return nil
				}
			}
			if err != nil {
				failures = append(failures, fmt.Errorf("shared daemon: %w", err))
			}
		} else {
			failures = append(failures, errNoDaemon)
		}
		if c.cfg.Mode == "daemon" {
			return errors.Join(failures...)
		}
	}

	resolved, err := resolveCodexBinary(c.cfg.CodexBin)
	if err != nil {
		failures = append(failures, err)
		return errors.Join(failures...)
	}
	extraEnv := append([]string(nil), resolved.ExtraEnv...)
	if c.cfg.CodexHome != "" {
		extraEnv = append(extraEnv, "CODEX_HOME="+c.cfg.CodexHome)
	}
	transport, err := startStdioTransport(resolved.Path, extraEnv, c.cfg.Logger)
	if err != nil {
		failures = append(failures, err)
		return errors.Join(failures...)
	}
	if err := c.activate(ctx, transport, resolved); err != nil {
		failures = append(failures, err)
		return errors.Join(failures...)
	}
	return nil
}

func (c *Client) activate(ctx context.Context, transport messageTransport, resolved resolvedBinary) error {
	c.mu.Lock()
	c.generation++
	generation := c.generation
	c.current = transport
	c.ready = false
	c.transportKind = transport.Kind()
	c.resolved = resolved
	c.lastActivity = time.Now()
	c.mu.Unlock()
	go c.readLoop(transport, generation)

	initialize, _ := json.Marshal(map[string]any{
		"clientInfo": map[string]any{
			"name":    "codex_remote_lite",
			"title":   "Codex Remote Lite",
			"version": "0.3.0",
		},
		"capabilities": map[string]any{
			"experimentalApi":                true,
			"requestAttestation":             false,
			"mcpServerOpenaiFormElicitation": false,
		},
	})
	initCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	_, rpcErr, err := c.callOn(initCtx, transport, generation, "initialize", initialize)
	if err == nil && rpcErr != nil {
		err = rpcErr
	}
	if err != nil {
		c.disconnectLocked(transport, generation, "initialization failed")
		return err
	}
	if err := c.notifyOn(transport, generation, "initialized", json.RawMessage(`{}`)); err != nil {
		c.disconnectLocked(transport, generation, "initialization acknowledgement failed")
		return err
	}
	c.mu.Lock()
	if c.current != transport || c.generation != generation {
		c.mu.Unlock()
		return errTransportClosed
	}
	c.ready = true
	c.mu.Unlock()
	c.cfg.Logger.Printf("connected to Codex app-server via %s", transport.Kind())
	c.emitStatus("connected")
	return nil
}

func (c *Client) callOn(ctx context.Context, transport messageTransport, generation uint64, method string, params json.RawMessage) (json.RawMessage, *RPCError, error) {
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	c.mu.Lock()
	if c.current != transport || c.generation != generation {
		c.mu.Unlock()
		return nil, nil, errTransportClosed
	}
	c.nextID++
	id := c.nextID
	replyCh := make(chan rpcReply, 1)
	c.pending[id] = replyCh
	c.lastActivity = time.Now()
	c.mu.Unlock()

	request := struct {
		ID     int64           `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{ID: id, Method: method, Params: params}
	message, err := json.Marshal(request)
	if err == nil {
		err = transport.WriteMessage(message)
	}
	if err != nil {
		c.removePending(id)
		go c.disconnect(transport, generation, "write failed")
		return nil, nil, err
	}

	select {
	case reply := <-replyCh:
		return reply.Result, reply.Error, reply.Cause
	case <-ctx.Done():
		c.removePending(id)
		return nil, nil, ctx.Err()
	}
}

func (c *Client) notifyOn(transport messageTransport, generation uint64, method string, params json.RawMessage) error {
	c.mu.Lock()
	if c.current != transport || c.generation != generation {
		c.mu.Unlock()
		return errTransportClosed
	}
	c.lastActivity = time.Now()
	c.mu.Unlock()
	message, err := json.Marshal(struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params,omitempty"`
	}{Method: method, Params: params})
	if err != nil {
		return err
	}
	return transport.WriteMessage(message)
}

func (c *Client) readLoop(transport messageTransport, generation uint64) {
	for {
		message, err := transport.ReadMessage()
		if err != nil {
			c.disconnect(transport, generation, "connection closed")
			return
		}
		var envelope rpcEnvelope
		if err := json.Unmarshal(message, &envelope); err != nil {
			c.cfg.Logger.Printf("ignored malformed app-server message: %v", err)
			continue
		}
		c.mu.Lock()
		if c.current != transport || c.generation != generation {
			c.mu.Unlock()
			return
		}
		c.lastActivity = time.Now()
		c.mu.Unlock()

		if len(envelope.ID) > 0 && envelope.Method == "" {
			c.handleReply(envelope)
			continue
		}
		if len(envelope.ID) > 0 && envelope.Method != "" {
			c.handleServerRequest(envelope, transport, generation)
			continue
		}
		if envelope.Method != "" {
			c.handleNotification(envelope, transport, generation)
		}
	}
}

func (c *Client) handleReply(envelope rpcEnvelope) {
	var id int64
	if err := json.Unmarshal(envelope.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if !ok {
		return
	}
	reply := rpcReply{Result: envelope.Result}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		var rpcErr RPCError
		if err := json.Unmarshal(envelope.Error, &rpcErr); err != nil {
			reply.Cause = fmt.Errorf("invalid app-server error: %s", envelope.Error)
		} else {
			reply.Error = &rpcErr
		}
	}
	ch <- reply
}

func (c *Client) handleServerRequest(envelope rpcEnvelope, transport messageTransport, generation uint64) {
	if !isRemotelyHandledServerRequest(envelope.Method) {
		c.rejectServerRequest(envelope.ID, generation, -32601, "server request is not exposed remotely")
		return
	}
	threadID := serverRequestThreadID(envelope.Params)
	c.mu.Lock()
	if c.current != transport || c.generation != generation {
		c.mu.Unlock()
		return
	}
	_, authorized := c.authorizedThreads[threadID]
	if !authorized && threadID != "" {
		if candidate := c.childCandidates[threadID]; candidate != nil && candidate.Generation == generation {
			queued := c.queueChildMessageLocked(candidate, envelope, true)
			c.mu.Unlock()
			if !queued {
				c.rejectServerRequest(envelope.ID, generation, -32000, "child verification backlog limit reached")
			}
			return
		}
	}
	var fileApproval *fileApprovalEvidence
	if authorized && envelope.Method == "item/fileChange/requestApproval" {
		if approvalKey, _, err := fileApprovalKeyFromRequest(envelope.Params, generation); err == nil {
			fileApproval = c.getFileApprovalLocked(approvalKey, time.Now())
		}
	}
	c.mu.Unlock()
	if threadID == "" || !authorized {
		c.rejectServerRequest(envelope.ID, generation, -32001, "thread is not authorized for remote control")
		return
	}
	var seed [16]byte
	if _, err := rand.Read(seed[:]); err != nil {
		c.rejectServerRequest(envelope.ID, generation, -32603, "could not allocate a request key")
		return
	}
	key := hex.EncodeToString(seed[:])
	canAccept, policyReason := serverRequestAcceptanceWithChecks(envelope.Method, envelope.Params, c.workspaceChecker(), c.cfg.CheckPath)
	request := pendingServerRequest{
		PublicRequest: PublicRequest{
			Key: key, Method: envelope.Method, Params: cloneRaw(envelope.Params),
			CanAccept: canAccept, PolicyReason: policyReason,
		},
		ID:           cloneRaw(envelope.ID),
		Generation:   generation,
		FileApproval: fileApproval,
	}
	if envelope.Method == "item/fileChange/requestApproval" {
		if fileApproval != nil {
			request.FileChanges = publicFileApprovalChanges(fileApproval)
		}
		request.CanAccept, request.PolicyReason = validateFileApprovalEvidenceWithChecks(request, time.Now(), c.workspaceChecker(), c.cfg.CheckPath)
	}
	c.mu.Lock()
	if c.current != transport || c.generation != generation {
		c.mu.Unlock()
		return
	}
	if request.FileApproval != nil {
		current := c.getFileApprovalLocked(request.FileApproval.Key, time.Now())
		if !sameFileApprovalEvidence(current, request.FileApproval) || !c.fileApprovalWorkspaceMatchesLocked(request.FileApproval) {
			request.FileApproval = nil
			request.FileChanges = nil
			request.CanAccept = false
			request.PolicyReason = "文件变更清单在审批展示前已失效"
		}
	}
	requestSize := pendingServerRequestBytes(request)
	if len(c.requests) >= maxPendingServerRequests || c.requestBytes+requestSize > maxPendingRequestBytes {
		c.mu.Unlock()
		c.cfg.Emit(map[string]any{"type": "reset", "reason": "server_request_backlog_limit"})
		c.rejectServerRequest(envelope.ID, generation, -32000, "remote request backlog limit reached")
		return
	}
	request.Timer = time.AfterFunc(serverRequestTimeout(request.Method, request.Params), func() {
		c.expireServerRequest(key, generation)
	})
	c.requests[key] = request
	c.requestBytes += requestSize
	c.cfg.Emit(map[string]any{"type": "server_request", "request": request.PublicRequest})
	c.mu.Unlock()
}

func (c *Client) AuthorizeThread(threadID string) {
	if threadID == "" {
		return
	}
	c.mu.Lock()
	c.authorizeThreadLocked(threadID)
	c.mu.Unlock()
}

func (c *Client) authorizeThreadLocked(threadID string) {
	if c.authorizedThreads == nil {
		c.authorizedThreads = make(map[string]struct{})
	}
	if _, exists := c.authorizedThreads[threadID]; exists {
		return
	}
	if len(c.authorizedOrder) >= maxAuthorizedThreads {
		evicted := c.authorizedOrder[0]
		delete(c.authorizedThreads, evicted)
		c.deleteFileApprovalsForThreadLocked(evicted)
		c.deleteThreadWorkspaceLocked(evicted)
		c.authorizedOrder = c.authorizedOrder[1:]
	}
	c.authorizedThreads[threadID] = struct{}{}
	c.authorizedOrder = append(c.authorizedOrder, threadID)
}

func (c *Client) revokeThreadLocked(threadID string) []pendingServerRequest {
	delete(c.authorizedThreads, threadID)
	for index, authorized := range c.authorizedOrder {
		if authorized != threadID {
			continue
		}
		copy(c.authorizedOrder[index:], c.authorizedOrder[index+1:])
		c.authorizedOrder = c.authorizedOrder[:len(c.authorizedOrder)-1]
		break
	}
	c.deleteFileApprovalsForThreadLocked(threadID)
	c.deleteThreadWorkspaceLocked(threadID)
	for key := range c.activeTurns {
		if len(key) > len(threadID) && key[:len(threadID)] == threadID && key[len(threadID)] == '/' {
			delete(c.activeTurns, key)
		}
	}
	var revoked []pendingServerRequest
	for key, request := range c.requests {
		if serverRequestThreadID(request.Params) != threadID {
			continue
		}
		delete(c.requests, key)
		c.requestBytes -= pendingServerRequestBytes(request)
		if request.Timer != nil {
			request.Timer.Stop()
		}
		revoked = append(revoked, request)
	}
	if c.requestBytes < 0 {
		c.requestBytes = 0
	}
	return revoked
}

func (c *Client) handleNotification(envelope rpcEnvelope, transport messageTransport, generation uint64) {
	threadID, turnID := notificationIDs(envelope.Params)
	key := threadID + "/" + turnID
	checkWorkspace := c.workspaceChecker()
	var startedThread childThreadInfo
	startedThreadValid := false
	if envelope.Method == "thread/started" {
		var err error
		startedThread, err = childThreadFromNotification(envelope.Params)
		startedThreadValid = err == nil && startedThread.ID != "" && startedThread.ID == threadID && startedThread.CWD != "" &&
			checkWorkspace != nil && checkWorkspace(startedThread.CWD) == nil
	}
	seeds := childSeedsFromNotification(envelope.Method, threadID, envelope.Params)
	startedSeed, startedCWD, startedOK := childSeedFromThreadStarted(envelope.Method, envelope.Params)
	if startedOK && (checkWorkspace == nil || checkWorkspace(startedCWD) != nil) {
		startedOK = false
	}
	var resolvedKeys []string
	var verify []childCandidateSeed
	c.mu.Lock()
	if c.current != transport || c.generation != generation {
		c.mu.Unlock()
		return
	}
	if threadID != "" {
		if _, authorized := c.authorizedThreads[threadID]; authorized {
			if envelope.Method == "thread/started" {
				if !startedThreadValid {
					revoked := c.revokeThreadLocked(threadID)
					c.mu.Unlock()
					for _, request := range revoked {
						c.rejectServerRequest(request.ID, generation, -32001, "thread working directory is no longer authorized")
						c.cfg.Emit(map[string]any{"type": "server_request_answered", "key": request.Key, "reason": "thread_revoked"})
					}
					return
				}
				c.putThreadWorkspaceLocked(threadID, threadWorkspace{Generation: generation, CWD: startedThread.CWD})
			}
		} else {
			if candidate := c.childCandidates[threadID]; candidate != nil && candidate.Generation == generation {
				c.queueChildMessageLocked(candidate, envelope, false)
				c.mu.Unlock()
				return
			}
			if startedOK && startedSeed.ChildID == threadID {
				if _, parentAuthorized := c.authorizedThreads[startedSeed.ParentID]; parentAuthorized {
					if candidate, created := c.addChildCandidateLocked(startedSeed, generation); candidate != nil {
						c.queueChildMessageLocked(candidate, envelope, false)
						if created {
							verify = append(verify, startedSeed)
						}
					}
				}
			}
			c.mu.Unlock()
			c.startChildVerifications(transport, generation, verify)
			return
		}
	} else if !allowedGlobalNotification(envelope.Method) {
		c.mu.Unlock()
		return
	}
	if envelope.Method == "item/started" {
		if evidence, err := parseFileApprovalItemStarted(envelope.Params, generation, time.Now()); err == nil {
			evidence.CWD = c.fileApprovalWorkspaceLocked(evidence.Key.ThreadID, generation)
			c.putFileApprovalLocked(evidence)
		}
	}
	switch envelope.Method {
	case "turn/started":
		if threadID != "" && turnID != "" {
			c.activeTurns[key] = struct{}{}
		}
	case "turn/completed":
		delete(c.activeTurns, key)
		c.deleteFileApprovalsForTurnLocked(generation, threadID, turnID)
	case "item/completed":
		if itemID := notificationItemID(envelope.Params); itemID != "" {
			c.deleteFileApprovalLocked(fileApprovalKey{
				Generation: generation, ThreadID: threadID, TurnID: turnID, ItemID: itemID,
			})
		}
	case "serverRequest/resolved":
		var params struct {
			RequestID any `json:"requestId"`
		}
		if json.Unmarshal(envelope.Params, &params) == nil {
			wanted := fmt.Sprint(params.RequestID)
			for publicKey, request := range c.requests {
				if rawIDString(request.ID) == wanted {
					delete(c.requests, publicKey)
					c.requestBytes -= pendingServerRequestBytes(request)
					if request.FileApproval != nil {
						c.deleteFileApprovalLocked(request.FileApproval.Key)
					}
					if request.Timer != nil {
						request.Timer.Stop()
					}
					resolvedKeys = append(resolvedKeys, publicKey)
				}
			}
		}
	}
	for _, seed := range seeds {
		if seed.ParentID != threadID || seed.ChildID == "" || seed.ChildID == threadID {
			continue
		}
		if _, parentAuthorized := c.authorizedThreads[seed.ParentID]; !parentAuthorized {
			continue
		}
		if _, childAuthorized := c.authorizedThreads[seed.ChildID]; childAuthorized {
			continue
		}
		if _, created := c.addChildCandidateLocked(seed, generation); created {
			verify = append(verify, seed)
		}
	}
	for _, publicKey := range resolvedKeys {
		// The browser only sees opaque request keys. Never leak or ask it to
		// correlate the app-server's raw JSON-RPC request ID.
		c.cfg.Emit(map[string]any{"type": "server_request_answered", "key": publicKey})
	}
	c.cfg.Emit(map[string]any{
		"type":        "notification",
		"method":      envelope.Method,
		"params":      json.RawMessage(envelope.Params),
		"emittedAtMs": envelope.EmittedAtMS,
	})
	c.mu.Unlock()
	c.startChildVerifications(transport, generation, verify)
}

func notificationIDs(raw json.RawMessage) (threadID, turnID string) {
	var params struct {
		ThreadID       string `json:"threadId"`
		ConversationID string `json:"conversationId"`
		TurnID         string `json:"turnId"`
		Turn           struct {
			ID string `json:"id"`
		} `json:"turn"`
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(raw, &params) != nil {
		return "", ""
	}
	if params.TurnID == "" {
		params.TurnID = params.Turn.ID
	}
	if params.ThreadID == "" {
		params.ThreadID = params.ConversationID
	}
	if params.ThreadID == "" {
		params.ThreadID = params.Thread.ID
	}
	return params.ThreadID, params.TurnID
}

func childSeedsFromNotification(method, parentID string, raw json.RawMessage) []childCandidateSeed {
	if parentID == "" {
		return nil
	}
	var items []json.RawMessage
	switch method {
	case "item/started", "item/completed":
		var params struct {
			Item json.RawMessage `json:"item"`
		}
		if json.Unmarshal(raw, &params) != nil || len(params.Item) == 0 {
			return nil
		}
		items = append(items, params.Item)
	case "turn/started", "turn/completed":
		var params struct {
			Turn struct {
				Items []json.RawMessage `json:"items"`
			} `json:"turn"`
		}
		if json.Unmarshal(raw, &params) != nil {
			return nil
		}
		items = params.Turn.Items
	default:
		return nil
	}

	seen := make(map[string]int)
	var seeds []childCandidateSeed
	for _, item := range items {
		for _, seed := range childSeedsFromItem(parentID, item) {
			if seed.ChildID == "" || seed.ChildID == parentID {
				continue
			}
			if index, ok := seen[seed.ChildID]; ok {
				if seeds[index].AgentPath == "" && seed.AgentPath != "" {
					seeds[index].AgentPath = seed.AgentPath
				}
				continue
			}
			seen[seed.ChildID] = len(seeds)
			seeds = append(seeds, seed)
		}
	}
	return seeds
}

func childSeedsFromItem(parentID string, raw json.RawMessage) []childCandidateSeed {
	var header struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &header) != nil {
		return nil
	}
	switch header.Type {
	case "subAgentActivity":
		var item struct {
			AgentThreadID string `json:"agentThreadId"`
			AgentPath     string `json:"agentPath"`
		}
		if json.Unmarshal(raw, &item) != nil || item.AgentThreadID == "" {
			return nil
		}
		return []childCandidateSeed{{ParentID: parentID, ChildID: item.AgentThreadID, AgentPath: item.AgentPath}}

	case "collabAgentToolCall":
		var item struct {
			SenderThreadID    string                     `json:"senderThreadId"`
			ReceiverThreadIDs []string                   `json:"receiverThreadIds"`
			AgentsStates      map[string]json.RawMessage `json:"agentsStates"`
		}
		if json.Unmarshal(raw, &item) != nil || item.SenderThreadID != parentID {
			return nil
		}
		children := append([]string(nil), item.ReceiverThreadIDs...)
		for childID := range item.AgentsStates {
			children = append(children, childID)
		}
		return seedsForChildren(parentID, children)

	case "collabToolCall":
		// Older app-server builds used a singular receiver/new-thread shape.
		// Keep it narrowly typed and require the item to name the authorized
		// parent as sender; arbitrary string fields never become candidates.
		var item struct {
			SenderThreadID    string                     `json:"senderThreadId"`
			ReceiverThreadID  string                     `json:"receiverThreadId"`
			NewThreadID       string                     `json:"newThreadId"`
			AgentThreadID     string                     `json:"agentThreadId"`
			ReceiverThreadIDs []string                   `json:"receiverThreadIds"`
			AgentsStates      map[string]json.RawMessage `json:"agentsStates"`
		}
		if json.Unmarshal(raw, &item) != nil || item.SenderThreadID != parentID {
			return nil
		}
		children := append([]string(nil), item.ReceiverThreadIDs...)
		children = append(children, item.ReceiverThreadID, item.NewThreadID, item.AgentThreadID)
		for childID := range item.AgentsStates {
			children = append(children, childID)
		}
		return seedsForChildren(parentID, children)
	default:
		return nil
	}
}

func seedsForChildren(parentID string, children []string) []childCandidateSeed {
	seen := make(map[string]struct{}, len(children))
	seeds := make([]childCandidateSeed, 0, len(children))
	for _, childID := range children {
		if childID == "" || childID == parentID {
			continue
		}
		if _, ok := seen[childID]; ok {
			continue
		}
		seen[childID] = struct{}{}
		seeds = append(seeds, childCandidateSeed{ParentID: parentID, ChildID: childID})
	}
	return seeds
}

type childThreadInfo struct {
	ID             string          `json:"id"`
	ParentThreadID string          `json:"parentThreadId"`
	CWD            string          `json:"cwd"`
	Source         json.RawMessage `json:"source"`
}

func childSeedFromThreadStarted(method string, raw json.RawMessage) (childCandidateSeed, string, bool) {
	if method != "thread/started" {
		return childCandidateSeed{}, "", false
	}
	info, err := childThreadFromNotification(raw)
	if err != nil {
		return childCandidateSeed{}, "", false
	}
	parentID, agentPath, ok := directThreadSpawnSource(info.Source)
	if !ok || info.ID == "" || info.ParentThreadID == "" || info.ParentThreadID != parentID || info.ID == parentID || info.CWD == "" {
		return childCandidateSeed{}, "", false
	}
	return childCandidateSeed{ParentID: parentID, ChildID: info.ID, AgentPath: agentPath}, info.CWD, true
}

func childThreadFromNotification(raw json.RawMessage) (childThreadInfo, error) {
	var params struct {
		Thread childThreadInfo `json:"thread"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return childThreadInfo{}, err
	}
	return params.Thread, nil
}

func childThreadFromReadResult(raw json.RawMessage) (childThreadInfo, error) {
	var result struct {
		Thread childThreadInfo `json:"thread"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return childThreadInfo{}, err
	}
	return result.Thread, nil
}

func directThreadSpawnSource(raw json.RawMessage) (parentID, agentPath string, ok bool) {
	var source struct {
		SubAgent json.RawMessage `json:"subAgent"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &source) != nil || len(source.SubAgent) == 0 {
		return "", "", false
	}
	var subAgent struct {
		ThreadSpawn *struct {
			ParentThreadID string  `json:"parent_thread_id"`
			AgentPath      *string `json:"agent_path"`
		} `json:"thread_spawn"`
	}
	if json.Unmarshal(source.SubAgent, &subAgent) != nil || subAgent.ThreadSpawn == nil || subAgent.ThreadSpawn.ParentThreadID == "" {
		return "", "", false
	}
	if subAgent.ThreadSpawn.AgentPath != nil {
		agentPath = *subAgent.ThreadSpawn.AgentPath
	}
	return subAgent.ThreadSpawn.ParentThreadID, agentPath, true
}

func validateChildThreadResult(raw json.RawMessage, seed childCandidateSeed, checkWorkspace func(string) error) error {
	if checkWorkspace == nil {
		return errors.New("receiver has no path authorization check")
	}
	info, err := childThreadFromReadResult(raw)
	if err != nil {
		return fmt.Errorf("invalid thread/read result: %w", err)
	}
	if info.ID != seed.ChildID || info.ParentThreadID != seed.ParentID || info.ID == "" || info.ID == info.ParentThreadID {
		return errors.New("thread/read returned a different parent-child relationship")
	}
	parentID, agentPath, ok := directThreadSpawnSource(info.Source)
	if !ok || parentID != seed.ParentID {
		return errors.New("thread/read did not confirm a direct subagent spawn")
	}
	if seed.AgentPath != "" && agentPath != seed.AgentPath {
		return errors.New("thread/read returned a different agent path")
	}
	if info.CWD == "" || checkWorkspace(info.CWD) != nil {
		return errors.New("child thread working directory is not authorized")
	}
	return nil
}

func cloneEnvelope(envelope rpcEnvelope) rpcEnvelope {
	var emittedAtMS *int64
	if envelope.EmittedAtMS != nil {
		value := *envelope.EmittedAtMS
		emittedAtMS = &value
	}
	return rpcEnvelope{
		ID:          cloneRaw(envelope.ID),
		Method:      envelope.Method,
		Params:      cloneRaw(envelope.Params),
		Result:      cloneRaw(envelope.Result),
		Error:       cloneRaw(envelope.Error),
		EmittedAtMS: emittedAtMS,
	}
}

func childMessageBytes(envelope rpcEnvelope) int {
	return len(envelope.ID) + len(envelope.Method) + len(envelope.Params) + len(envelope.Result) + len(envelope.Error)
}

func (c *Client) addChildCandidateLocked(seed childCandidateSeed, generation uint64) (*childCandidate, bool) {
	if seed.ParentID == "" || seed.ChildID == "" || seed.ParentID == seed.ChildID || c.current == nil || c.generation != generation {
		return nil, false
	}
	if _, ok := c.authorizedThreads[seed.ParentID]; !ok {
		return nil, false
	}
	if _, ok := c.authorizedThreads[seed.ChildID]; ok {
		return nil, false
	}
	if existing := c.childCandidates[seed.ChildID]; existing != nil {
		if existing.Generation == generation && existing.ParentID == seed.ParentID &&
			(existing.AgentPath == "" || seed.AgentPath == "" || existing.AgentPath == seed.AgentPath) {
			return existing, false
		}
		return nil, false
	}
	if len(c.childCandidates) >= maxChildCandidates {
		return nil, false
	}
	if c.childCandidates == nil {
		c.childCandidates = make(map[string]*childCandidate)
	}
	candidate := &childCandidate{childCandidateSeed: seed, Generation: generation}
	c.childCandidates[seed.ChildID] = candidate
	return candidate, true
}

func (c *Client) queueChildMessageLocked(candidate *childCandidate, envelope rpcEnvelope, serverRequest bool) bool {
	if candidate == nil || c.childCandidates[candidate.ChildID] != candidate {
		return false
	}
	bytes := childMessageBytes(envelope)
	if c.childEventCount >= maxChildCandidateEvents || bytes > maxChildCandidateBytes || c.childEventBytes > maxChildCandidateBytes-bytes {
		return false
	}
	candidate.Queue = append(candidate.Queue, queuedChildMessage{
		Envelope: cloneEnvelope(envelope), ServerRequest: serverRequest, Bytes: bytes,
	})
	c.childEventCount++
	c.childEventBytes += bytes
	return true
}

func (c *Client) startChildVerifications(transport messageTransport, generation uint64, seeds []childCandidateSeed) {
	for _, seed := range seeds {
		seed := seed
		go c.verifyChildCandidate(transport, generation, seed)
	}
}

func (c *Client) verifyChildCandidate(transport messageTransport, generation uint64, seed childCandidateSeed) {
	c.mu.Lock()
	candidate := c.childCandidates[seed.ChildID]
	validCandidate := c.current == transport && c.generation == generation && candidate != nil &&
		candidate.Generation == generation && candidate.ParentID == seed.ParentID && candidate.AgentPath == seed.AgentPath
	c.mu.Unlock()
	if !validCandidate {
		return
	}

	params, _ := json.Marshal(map[string]any{"threadId": seed.ChildID, "includeTurns": false})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, rpcErr, err := c.callOn(ctx, transport, generation, "thread/read", params)
	if err == nil && rpcErr != nil {
		err = rpcErr
	}
	if err == nil {
		err = validateChildThreadResult(result, seed, c.workspaceChecker())
	}
	if err == nil {
		c.captureThreadWorkspaces("thread/read", result, generation)
	}
	c.finishChildVerification(transport, generation, seed, err)
}

func (c *Client) finishChildVerification(transport messageTransport, generation uint64, seed childCandidateSeed, verificationErr error) {
	c.mu.Lock()
	candidate := c.childCandidates[seed.ChildID]
	if c.current != transport || c.generation != generation || candidate == nil || candidate.Generation != generation ||
		candidate.ParentID != seed.ParentID || candidate.AgentPath != seed.AgentPath {
		c.mu.Unlock()
		return
	}
	if _, parentAuthorized := c.authorizedThreads[seed.ParentID]; !parentAuthorized {
		verificationErr = errors.New("parent thread is no longer authorized")
	}
	delete(c.childCandidates, seed.ChildID)
	queue := append([]queuedChildMessage(nil), candidate.Queue...)
	for _, message := range candidate.Queue {
		c.childEventCount--
		c.childEventBytes -= message.Bytes
	}
	if c.childEventCount < 0 {
		c.childEventCount = 0
	}
	if c.childEventBytes < 0 {
		c.childEventBytes = 0
	}
	if verificationErr == nil {
		c.authorizeThreadLocked(seed.ChildID)
	}
	c.mu.Unlock()

	if verificationErr != nil {
		if c.cfg.Logger != nil {
			c.cfg.Logger.Printf("rejected subagent thread relationship: %v", verificationErr)
		}
		for _, message := range queue {
			if message.ServerRequest {
				c.rejectServerRequest(message.Envelope.ID, generation, -32001, "child thread relationship could not be verified")
			}
		}
		return
	}
	for _, message := range queue {
		if message.ServerRequest {
			c.handleServerRequest(message.Envelope, transport, generation)
		} else {
			c.handleNotification(message.Envelope, transport, generation)
		}
	}
}

func allowedGlobalNotification(method string) bool {
	return method == "account/rateLimits/updated"
}

func rawIDString(raw json.RawMessage) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return string(raw)
	}
	return fmt.Sprint(value)
}

func (c *Client) PendingRequests() []PublicRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	requests := make([]PublicRequest, 0, len(c.requests))
	for _, request := range c.requests {
		requests = append(requests, PublicRequest{
			Key: request.Key, Method: request.Method, Params: cloneRaw(request.Params), FileChanges: cloneRaw(request.FileChanges),
			CanAccept: request.CanAccept, PolicyReason: request.PolicyReason,
		})
	}
	return requests
}

func (c *Client) Respond(_ context.Context, key string, result json.RawMessage) error {
	c.mu.Lock()
	request, ok := c.requests[key]
	transport := c.current
	if !ok || transport == nil || request.Generation != c.generation {
		c.mu.Unlock()
		return os.ErrNotExist
	}
	c.mu.Unlock()

	sanitized, err := sanitizeServerResponseWithChecks(request, result, c.workspaceChecker(), c.cfg.CheckPath)
	if err != nil {
		return err
	}

	c.mu.Lock()
	current, ok := c.requests[key]
	transport = c.current
	if !ok || transport == nil || current.Generation != c.generation || rawIDString(current.ID) != rawIDString(request.ID) {
		c.mu.Unlock()
		return os.ErrNotExist
	}
	if current.Method == "item/fileChange/requestApproval" && fileApprovalResultAccepted(sanitized) {
		if current.FileApproval == nil {
			c.mu.Unlock()
			return fmt.Errorf("%w: 文件变更清单已失效", ErrInvalidServerResponse)
		}
		cached := c.getFileApprovalLocked(current.FileApproval.Key, time.Now())
		if !sameFileApprovalEvidence(cached, current.FileApproval) || !c.fileApprovalWorkspaceMatchesLocked(current.FileApproval) {
			c.mu.Unlock()
			return fmt.Errorf("%w: 文件变更清单已失效或被替换", ErrInvalidServerResponse)
		}
	}
	// Claim the request while holding the lock. A second browser now receives a
	// conflict instead of sending a duplicate approval to app-server.
	delete(c.requests, key)
	c.requestBytes -= pendingServerRequestBytes(current)
	if current.FileApproval != nil {
		c.deleteFileApprovalLocked(current.FileApproval.Key)
	}
	if current.Timer != nil {
		current.Timer.Stop()
	}
	message, err := json.Marshal(struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}{ID: current.ID, Result: sanitized})
	if err == nil {
		c.lastActivity = time.Now()
	}
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if err := transport.WriteMessage(message); err != nil {
		go c.disconnect(transport, current.Generation, "server response write failed")
		return err
	}
	c.cfg.Emit(map[string]any{"type": "server_request_answered", "key": key})
	return nil
}

func (c *Client) rejectServerRequest(id json.RawMessage, generation uint64, code int, message string) {
	c.mu.Lock()
	transport := c.current
	if transport == nil || c.generation != generation {
		c.mu.Unlock()
		return
	}
	c.lastActivity = time.Now()
	c.mu.Unlock()
	if err := writeServerError(transport, id, code, message); err != nil {
		go c.disconnect(transport, generation, "server rejection write failed")
	}
}

func (c *Client) expireServerRequest(key string, generation uint64) {
	c.mu.Lock()
	request, ok := c.requests[key]
	transport := c.current
	if !ok || transport == nil || c.generation != generation || request.Generation != generation {
		c.mu.Unlock()
		return
	}
	delete(c.requests, key)
	c.requestBytes -= pendingServerRequestBytes(request)
	if request.FileApproval != nil {
		c.deleteFileApprovalLocked(request.FileApproval.Key)
	}
	c.lastActivity = time.Now()
	c.mu.Unlock()

	result, useError := timeoutResult(request.Method)
	var err error
	if useError {
		err = writeServerError(transport, request.ID, -32000, "remote request timed out")
	} else {
		err = writeServerResult(transport, request.ID, result)
	}
	if err != nil {
		go c.disconnect(transport, generation, "server timeout response write failed")
	}
	c.cfg.Emit(map[string]any{"type": "server_request_answered", "key": key, "reason": "timeout"})
}

func writeServerResult(transport messageTransport, id, result json.RawMessage) error {
	message, err := json.Marshal(struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
	}{ID: id, Result: result})
	if err != nil {
		return err
	}
	return transport.WriteMessage(message)
}

func writeServerError(transport messageTransport, id json.RawMessage, code int, text string) error {
	message, err := json.Marshal(struct {
		ID    json.RawMessage `json:"id"`
		Error RPCError        `json:"error"`
	}{ID: id, Error: RPCError{Code: code, Message: text}})
	if err != nil {
		return err
	}
	return transport.WriteMessage(message)
}

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Status{
		Connected:       c.current != nil && c.ready,
		Transport:       c.transportKind,
		Generation:      c.generation,
		ActiveTurns:     len(c.activeTurns),
		PendingRequests: len(c.requests),
		PendingReqBytes: c.requestBytes,
		PendingRPCs:     len(c.pending),
		CodexBinary:     c.resolved.Path,
		NativeBinary:    c.resolved.Native,
		IdleTimeoutSec:  int64(c.cfg.IdleTimeout.Seconds()),
	}
}

func (c *Client) removePending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) disconnect(transport messageTransport, generation uint64, reason string) {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	c.disconnectLocked(transport, generation, reason)
}

// disconnectLocked requires startMu. Keeping lifecycle mutation under one lock
// prevents a reconnect from racing the old reader loop's final EOF.
func (c *Client) disconnectLocked(transport messageTransport, generation uint64, reason string) {
	c.mu.Lock()
	if c.current != transport || c.generation != generation {
		c.mu.Unlock()
		return
	}
	c.current = nil
	c.ready = false
	c.transportKind = ""
	c.resolved = resolvedBinary{}
	failed := c.pending
	requests := c.requests
	c.pending = make(map[int64]chan rpcReply)
	c.requests = make(map[string]pendingServerRequest)
	c.requestBytes = 0
	c.activeTurns = make(map[string]struct{})
	c.authorizedThreads = make(map[string]struct{})
	c.authorizedOrder = nil
	c.childCandidates = make(map[string]*childCandidate)
	c.childEventCount = 0
	c.childEventBytes = 0
	c.fileApprovals = make(map[fileApprovalKey]*fileApprovalEvidence)
	c.fileApprovalOrder = nil
	c.fileApprovalBytes = 0
	c.threadWorkspaces = make(map[string]threadWorkspace)
	c.threadWorkspaceOrder = nil
	c.mu.Unlock()
	_ = transport.Close()
	for _, request := range requests {
		if request.Timer != nil {
			request.Timer.Stop()
		}
	}
	for _, ch := range failed {
		ch <- rpcReply{Cause: errTransportClosed}
	}
	c.cfg.Logger.Printf("Codex app-server disconnected: %s", reason)
	c.emitStatus(reason)
}

func (c *Client) emitStatus(reason string) {
	c.cfg.Emit(map[string]any{
		"type":   "backend_status",
		"reason": reason,
		"status": c.Status(),
	})
}

func (c *Client) idleLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.mu.Lock()
			transport := c.current
			generation := c.generation
			idle := transport != nil && len(c.activeTurns) == 0 && len(c.requests) == 0 && len(c.pending) == 0 && time.Since(c.lastActivity) >= c.cfg.IdleTimeout
			c.mu.Unlock()
			if idle {
				c.disconnect(transport, generation, "idle timeout")
				// This process is intentionally tiny and often sits unused for
				// hours. Return transient JSON/RPC buffers to the OS at the one
				// lifecycle point where a short GC pause cannot affect a turn.
				debug.FreeOSMemory()
			}
		case <-c.idleStop:
			return
		}
	}
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	close(c.idleStop)
	transport := c.current
	generation := c.generation
	c.mu.Unlock()
	if transport != nil {
		c.disconnect(transport, generation, "receiver shutdown")
	}
	return nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

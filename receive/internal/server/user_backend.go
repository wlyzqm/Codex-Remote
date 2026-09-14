package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"codex-remote/internal/codex"
	"codex-remote/internal/policy"
)

// Every account shares the workstation's one app-server connection. This
// adapter owns only authorization and a small set of active turn identifiers.
type userBackend struct {
	turnMu   sync.Mutex
	child    *Server
	prepared map[string]uint64
	mu       sync.Mutex
	root     *Server
	user     User
	closed   bool
	turns    map[string]string
}

func (s *Server) UserFactory() UserBackendFactory {
	return func(u User, _ string, _ string, _ *policy.Paths, _ func(any)) (Backend, error) {
		return &userBackend{root: s, user: u, turns: map[string]string{}, prepared: map[string]uint64{}}, nil
	}
}
func (b *userBackend) owns(id string) bool {
	b.root.users.mu.Lock()
	defer b.root.users.mu.Unlock()
	return id != "" && b.root.users.owners[id] == b.user.ID
}
func (b *userBackend) Call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, *codex.RPCError, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, nil, errors.New("账户权限已更新，请重新登录")
	}
	id := policy.ThreadID(params)
	if id != "" && !b.owns(id) {
		return nil, nil, errors.New("无权访问此会话")
	}
	if method == "thread/list" {
		var input map[string]any
		if json.Unmarshal(params, &input) != nil {
			return nil, nil, errors.New("无效参数")
		}
		for _, field := range []string{"parentThreadId", "ancestorThreadId"} {
			if target, ok := input[field].(string); ok && !b.owns(target) {
				return nil, nil, errors.New("无权访问此会话")
			}
		}
	}
	if id != "" && (method == "turn/start" || method == "review/start" || method == "thread/goal/set" || method == "thread/compact/start") {
		generation := b.root.backend.Status().Generation
		if prepared, ok := b.prepared[id]; !ok || prepared != generation {
			if err := b.resume(ctx, id); err != nil {
				return nil, nil, err
			}
		}
	}
	result, rpcErr, err := b.root.backend.Call(ctx, method, params)
	if err == nil && rpcErr != nil && method == "turn/start" && strings.HasPrefix(rpcErr.Message, "thread not found:") {
		if err = b.resume(ctx, id); err != nil {
			return nil, nil, err
		}
		result, rpcErr, err = b.root.backend.Call(ctx, method, params)
	}
	if err != nil || rpcErr != nil {
		return result, rpcErr, err
	}
	if method == "thread/start" || method == "thread/fork" {
		created := resultThreadID(result)
		if created == "" {
			return nil, nil, errors.New("新会话缺少标识")
		}
		m := b.root.users
		m.mu.Lock()
		if owner, ok := m.owners[created]; ok && owner != b.user.ID {
			m.mu.Unlock()
			return nil, nil, errors.New("会话归属冲突")
		}
		m.owners[created] = b.user.ID
		data, e := json.Marshal(m.owners)
		if e == nil {
			e = privateWrite(filepath.Join(b.root.cfg.UserRoot, "threads.json"), data)
		}
		if e != nil {
			delete(m.owners, created)
		}
		m.mu.Unlock()
		if e != nil {
			return nil, nil, fmt.Errorf("无法保存会话归属: %w", e)
		}
	}
	if method == "thread/start" || method == "thread/fork" || method == "thread/resume" {
		if created := resultThreadID(result); created != "" {
			b.prepared[created] = b.root.backend.Status().Generation
		}
	}
	if method == "thread/list" {
		var envelope map[string]json.RawMessage
		if err = json.Unmarshal(result, &envelope); err != nil {
			return nil, nil, err
		}
		var threads []json.RawMessage
		if err = json.Unmarshal(envelope["data"], &threads); err != nil {
			return nil, nil, err
		}
		filtered := []json.RawMessage{}
		for _, raw := range threads {
			var thread struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &thread) == nil && b.owns(thread.ID) {
				filtered = append(filtered, raw)
			}
		}
		envelope["data"], _ = json.Marshal(filtered)
		result, err = json.Marshal(envelope)
	}
	if method == "turn/start" {
		var output struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(result, &output) == nil && output.Turn.ID != "" {
			b.turnMu.Lock()
			b.turns[id] = output.Turn.ID
			b.turnMu.Unlock()
		}
	}
	return result, rpcErr, err
}
func (b *userBackend) PendingRequests() []codex.PublicRequest {
	requests := []codex.PublicRequest{}
	for _, request := range b.root.backend.PendingRequests() {
		if b.owns(eventThread(request.Params)) {
			request.CanAccept = request.Method == "item/tool/requestUserInput"
			if !request.CanAccept {
				request.PolicyReason = "普通账户不能扩大沙箱权限"
			}
			requests = append(requests, request)
		}
	}
	return requests
}
func (b *userBackend) Respond(ctx context.Context, key string, result json.RawMessage) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("账户权限已更新")
	}
	for _, request := range b.PendingRequests() {
		if request.Key != key {
			continue
		}
		if request.Method != "item/tool/requestUserInput" {
			var response struct {
				Decision string `json:"decision"`
			}
			_ = json.Unmarshal(result, &response)
			if response.Decision != "decline" && response.Decision != "cancel" && response.Decision != "abort" {
				return codex.ErrInvalidServerResponse
			}
		}
		return b.root.backend.Respond(ctx, key, result)
	}
	return os.ErrNotExist
}
func (b *userBackend) Status() codex.Status {
	status := b.root.backend.Status()
	status.PendingRequests = len(b.PendingRequests())
	status.PendingReqBytes = 0
	status.PendingRPCs = 0
	b.turnMu.Lock()
	status.ActiveTurns = len(b.turns)
	b.turnMu.Unlock()
	return status
}
func (b *userBackend) AuthorizeThread(id string) {
	if b.owns(id) {
		b.root.backend.AuthorizeThread(id)
	}
}
func (b *userBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.turnMu.Lock()
	turns := make(map[string]string, len(b.turns))
	for id, turn := range b.turns {
		turns[id] = turn
	}
	b.turnMu.Unlock()
	for id, turn := range turns {
		params, _ := json.Marshal(map[string]any{"threadId": id, "turnId": turn})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _, _ = b.root.backend.Call(ctx, "turn/interrupt", params)
		cancel()
	}
	return nil
}
func eventThread(raw json.RawMessage) string {
	var p struct {
		ThreadID       string `json:"threadId"`
		ConversationID string `json:"conversationId"`
		Thread         struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	_ = json.Unmarshal(raw, &p)
	if p.ThreadID != "" {
		return p.ThreadID
	}
	if p.ConversationID != "" {
		return p.ConversationID
	}
	return p.Thread.ID
}
func (m *userManager) observe(data []byte) {
	var event struct {
		Type    string          `json:"type"`
		Method  string          `json:"method"`
		Key     string          `json:"key"`
		Params  json.RawMessage `json:"params"`
		Request struct {
			Key    string          `json:"key"`
			Params json.RawMessage `json:"params"`
		} `json:"request"`
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	thread := eventThread(event.Params)
	if event.Type == "server_request" {
		thread = eventThread(event.Request.Params)
	}
	m.mu.Lock()
	owner := m.owners[thread]
	if event.Type == "server_request" && owner != "" {
		m.requestOwners[event.Request.Key] = owner
	}
	if event.Type == "server_request_answered" {
		owner = m.requestOwners[event.Key]
		delete(m.requestOwners, event.Key)
	}
	child := m.servers[owner]
	m.mu.Unlock()
	if child != nil {
		if backend, ok := child.backend.(*userBackend); ok && (event.Method == "turn/started" || event.Method == "turn/completed") {
			var p struct {
				Turn struct {
					ID string `json:"id"`
				} `json:"turn"`
			}
			_ = json.Unmarshal(event.Params, &p)
			backend.turnMu.Lock()
			if event.Method == "turn/started" {
				backend.turns[thread] = p.Turn.ID
			} else {
				delete(backend.turns, thread)
			}
			backend.turnMu.Unlock()
		}
		// Do not forward raw approval events with the administrator's CanAccept flag.
		if event.Type == "server_request" {
			var value map[string]any
			_ = json.Unmarshal(data, &value)
			if request, ok := value["request"].(map[string]any); ok {
				request["canAccept"] = request["method"] == "item/tool/requestUserInput"
				request["policyReason"] = "普通账户不能扩大沙箱权限"
			}
			data, _ = json.Marshal(value)
		}
		child.broker.Publish(data)
		child.Observe(data)
	}
}

func (b *userBackend) resume(ctx context.Context, id string) error {
	params, _ := json.Marshal(map[string]any{"threadId": id})
	result, rpcErr, err := b.root.backend.Call(ctx, "thread/read", params)
	if err != nil {
		return err
	}
	if rpcErr != nil {
		return rpcErr
	}
	var info struct {
		Thread struct {
			CWD string `json:"cwd"`
		} `json:"thread"`
	}
	if json.Unmarshal(result, &info) != nil {
		return errors.New("会话工作区无效")
	}
	cwd, err := b.child.cfg.Paths.Check(info.Thread.CWD)
	if err != nil {
		return err
	}
	params, _ = json.Marshal(map[string]any{"threadId": id, "cwd": cwd})
	params, err = b.child.sanitizeUserParams("thread/resume", params)
	if err != nil {
		return err
	}
	_, rpcErr, err = b.root.backend.Call(ctx, "thread/resume", params)
	if err != nil {
		return err
	}
	if rpcErr != nil {
		return rpcErr
	}
	b.prepared[id] = b.root.backend.Status().Generation
	return nil
}

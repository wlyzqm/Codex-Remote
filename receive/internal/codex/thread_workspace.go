package codex

import (
	"encoding/json"
	"path/filepath"
)

const (
	maxThreadWorkspaces     = 512
	maxThreadWorkspaceBytes = 4096
)

type threadWorkspace struct {
	Generation uint64
	CWD        string
}

type threadWorkspaceCandidate struct {
	ID  string `json:"id"`
	CWD string `json:"cwd"`
}

func (c *Client) captureThreadWorkspaces(method string, result json.RawMessage, generation uint64) {
	checkWorkspace := c.workspaceChecker()
	if checkWorkspace == nil || len(result) == 0 {
		return
	}
	var candidates []threadWorkspaceCandidate
	switch method {
	case "thread/read", "thread/start", "thread/resume", "thread/fork":
		var envelope struct {
			Thread threadWorkspaceCandidate `json:"thread"`
		}
		if json.Unmarshal(result, &envelope) != nil {
			return
		}
		candidates = append(candidates, envelope.Thread)
	case "thread/list":
		var envelope struct {
			Data []threadWorkspaceCandidate `json:"data"`
		}
		if json.Unmarshal(result, &envelope) != nil {
			return
		}
		candidates = envelope.Data
	default:
		return
	}

	verified := make([]threadWorkspaceCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID == "" || candidate.CWD == "" || len(candidate.CWD) > maxThreadWorkspaceBytes ||
			!filepath.IsAbs(candidate.CWD) || checkWorkspace(candidate.CWD) != nil {
			continue
		}
		verified = append(verified, candidate)
	}
	if len(verified) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil || c.generation != generation {
		return
	}
	for _, candidate := range verified {
		c.putThreadWorkspaceLocked(candidate.ID, threadWorkspace{Generation: generation, CWD: candidate.CWD})
	}
}

func (c *Client) putThreadWorkspaceLocked(threadID string, workspace threadWorkspace) {
	if threadID == "" || workspace.Generation != c.generation || workspace.CWD == "" ||
		len(workspace.CWD) > maxThreadWorkspaceBytes || !filepath.IsAbs(workspace.CWD) {
		return
	}
	if c.threadWorkspaces == nil {
		c.threadWorkspaces = make(map[string]threadWorkspace)
	}
	if _, exists := c.threadWorkspaces[threadID]; exists {
		c.removeThreadWorkspaceOrderLocked(threadID)
	}
	for len(c.threadWorkspaces) >= maxThreadWorkspaces && len(c.threadWorkspaceOrder) > 0 {
		evicted := c.threadWorkspaceOrder[0]
		delete(c.threadWorkspaces, evicted)
		c.threadWorkspaceOrder = c.threadWorkspaceOrder[1:]
	}
	c.threadWorkspaces[threadID] = workspace
	c.threadWorkspaceOrder = append(c.threadWorkspaceOrder, threadID)
}

func (c *Client) removeThreadWorkspaceOrderLocked(threadID string) {
	for index, ordered := range c.threadWorkspaceOrder {
		if ordered != threadID {
			continue
		}
		copy(c.threadWorkspaceOrder[index:], c.threadWorkspaceOrder[index+1:])
		c.threadWorkspaceOrder = c.threadWorkspaceOrder[:len(c.threadWorkspaceOrder)-1]
		return
	}
}

func (c *Client) deleteThreadWorkspaceLocked(threadID string) {
	delete(c.threadWorkspaces, threadID)
	c.removeThreadWorkspaceOrderLocked(threadID)
}

func (c *Client) fileApprovalWorkspaceLocked(threadID string, generation uint64) string {
	workspace, ok := c.threadWorkspaces[threadID]
	if !ok || workspace.Generation != generation {
		return ""
	}
	return workspace.CWD
}

func (c *Client) fileApprovalWorkspaceMatchesLocked(evidence *fileApprovalEvidence) bool {
	if evidence == nil || evidence.Key.Generation != c.generation {
		return false
	}
	if evidence.CWD == "" {
		return true
	}
	return c.fileApprovalWorkspaceLocked(evidence.Key.ThreadID, evidence.Key.Generation) == evidence.CWD
}

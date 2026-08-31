package codex

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"
)

const (
	maxFileApprovalEntries    = 64
	maxFileApprovalBytes      = 2 << 20
	maxFileApprovalEntryBytes = 512 << 10
	maxFileApprovalChanges    = 2048
	fileApprovalTTL           = 10 * time.Minute
)

type fileApprovalKey struct {
	Generation uint64
	ThreadID   string
	TurnID     string
	ItemID     string
}

type fileApprovalChange struct {
	Path string                 `json:"path"`
	Diff string                 `json:"diff"`
	Kind fileApprovalChangeKind `json:"kind"`
}

type fileApprovalChangeKind struct {
	Type     string  `json:"type"`
	MovePath *string `json:"move_path,omitempty"`
}

type fileApprovalEvidence struct {
	Key         fileApprovalKey
	CWD         string
	Changes     []fileApprovalChange
	ChangesJSON json.RawMessage
	Digest      [sha256.Size]byte
	CapturedAt  time.Time
	Bytes       int
}

type fileApprovalRequestParams struct {
	ThreadID  string  `json:"threadId"`
	TurnID    string  `json:"turnId"`
	ItemID    string  `json:"itemId"`
	GrantRoot *string `json:"grantRoot"`
}

func parseFileApprovalItemStarted(params json.RawMessage, generation uint64, now time.Time) (*fileApprovalEvidence, error) {
	if len(params) == 0 || len(params) > maxFileApprovalEntryBytes {
		return nil, errors.New("file change notification is empty or too large")
	}
	var notification struct {
		ThreadID    string          `json:"threadId"`
		TurnID      string          `json:"turnId"`
		StartedAtMS *int64          `json:"startedAtMs"`
		Item        json.RawMessage `json:"item"`
	}
	if json.Unmarshal(params, &notification) != nil || notification.ThreadID == "" || notification.TurnID == "" ||
		notification.StartedAtMS == nil || len(notification.Item) == 0 {
		return nil, errors.New("invalid item/started notification")
	}
	var itemFields map[string]json.RawMessage
	if json.Unmarshal(notification.Item, &itemFields) != nil || !onlyJSONFields(itemFields, "type", "id", "status", "changes") {
		return nil, errors.New("invalid fileChange item fields")
	}
	var item struct {
		Type    string            `json:"type"`
		ID      string            `json:"id"`
		Status  string            `json:"status"`
		Changes []json.RawMessage `json:"changes"`
	}
	if json.Unmarshal(notification.Item, &item) != nil || item.Type != "fileChange" || item.ID == "" ||
		!validPatchApplyStatus(item.Status) || len(item.Changes) == 0 || len(item.Changes) > maxFileApprovalChanges {
		return nil, errors.New("invalid fileChange item")
	}

	changes := make([]fileApprovalChange, 0, len(item.Changes))
	for _, raw := range item.Changes {
		change, err := parseFileApprovalChange(raw)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	canonical, err := json.Marshal(changes)
	if err != nil || len(canonical) == 0 || len(canonical) > maxFileApprovalEntryBytes {
		return nil, errors.New("file change evidence is too large")
	}
	// The parsed strings and the canonical JSON both retain the diff content.
	// Count both copies plus conservative slice/object overhead.
	bytes := 2*len(canonical) + len(changes)*128
	if bytes > maxFileApprovalEntryBytes {
		return nil, errors.New("file change evidence exceeds the memory limit")
	}
	return &fileApprovalEvidence{
		Key: fileApprovalKey{
			Generation: generation,
			ThreadID:   notification.ThreadID,
			TurnID:     notification.TurnID,
			ItemID:     item.ID,
		},
		Changes:     changes,
		ChangesJSON: canonical,
		Digest:      sha256.Sum256(canonical),
		CapturedAt:  now,
		Bytes:       bytes,
	}, nil
}

func parseFileApprovalChange(raw json.RawMessage) (fileApprovalChange, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || !onlyJSONFields(fields, "path", "diff", "kind") {
		return fileApprovalChange{}, errors.New("invalid file update fields")
	}
	var change struct {
		Path string          `json:"path"`
		Diff *string         `json:"diff"`
		Kind json.RawMessage `json:"kind"`
	}
	if json.Unmarshal(raw, &change) != nil || change.Path == "" || change.Diff == nil || len(change.Kind) == 0 {
		return fileApprovalChange{}, errors.New("incomplete file update")
	}
	var kindFields map[string]json.RawMessage
	if json.Unmarshal(change.Kind, &kindFields) != nil || !onlyJSONFields(kindFields, "type", "move_path") {
		return fileApprovalChange{}, errors.New("invalid file update kind")
	}
	var kind struct {
		Type     string  `json:"type"`
		MovePath *string `json:"move_path"`
	}
	if json.Unmarshal(change.Kind, &kind) != nil {
		return fileApprovalChange{}, errors.New("invalid file update kind")
	}
	switch kind.Type {
	case "add", "delete":
		if _, exists := kindFields["move_path"]; exists {
			return fileApprovalChange{}, errors.New("move_path is only valid for update changes")
		}
	case "update":
		if kind.MovePath != nil && *kind.MovePath == "" {
			return fileApprovalChange{}, errors.New("move_path cannot be empty")
		}
	default:
		return fileApprovalChange{}, errors.New("unknown file update kind")
	}
	return fileApprovalChange{
		Path: change.Path,
		Diff: *change.Diff,
		Kind: fileApprovalChangeKind{Type: kind.Type, MovePath: kind.MovePath},
	}, nil
}

func validPatchApplyStatus(status string) bool {
	switch status {
	case "inProgress", "completed", "failed", "declined":
		return true
	default:
		return false
	}
}

func onlyJSONFields(fields map[string]json.RawMessage, allowed ...string) bool {
	if fields == nil {
		return false
	}
	wanted := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		wanted[field] = struct{}{}
	}
	for field := range fields {
		if _, ok := wanted[field]; !ok {
			return false
		}
	}
	return true
}

func fileApprovalKeyFromRequest(params json.RawMessage, generation uint64) (fileApprovalKey, *string, error) {
	var request fileApprovalRequestParams
	if json.Unmarshal(params, &request) != nil || request.ThreadID == "" || request.TurnID == "" || request.ItemID == "" {
		return fileApprovalKey{}, nil, errors.New("文件审批缺少 threadId、turnId 或 itemId")
	}
	return fileApprovalKey{
		Generation: generation,
		ThreadID:   request.ThreadID,
		TurnID:     request.TurnID,
		ItemID:     request.ItemID,
	}, request.GrantRoot, nil
}

func notificationItemID(params json.RawMessage) string {
	var notification struct {
		Item struct {
			ID string `json:"id"`
		} `json:"item"`
	}
	if json.Unmarshal(params, &notification) != nil {
		return ""
	}
	return notification.Item.ID
}

func pendingServerRequestBytes(request pendingServerRequest) int {
	return len(request.Params) + len(request.FileChanges)
}

func publicFileApprovalChanges(evidence *fileApprovalEvidence) json.RawMessage {
	if evidence == nil {
		return nil
	}
	type publicChange struct {
		Path string                 `json:"path"`
		Kind fileApprovalChangeKind `json:"kind"`
	}
	changes := make([]publicChange, 0, len(evidence.Changes))
	for _, change := range evidence.Changes {
		changes = append(changes, publicChange{Path: change.Path, Kind: change.Kind})
	}
	encoded, err := json.Marshal(changes)
	if err != nil {
		return nil
	}
	return encoded
}

func fileApprovalResultAccepted(result json.RawMessage) bool {
	var response struct {
		Decision string `json:"decision"`
	}
	return json.Unmarshal(result, &response) == nil && response.Decision == "accept"
}

func validateFileApprovalEvidenceWithChecks(request pendingServerRequest, now time.Time, checkWorkspace, checkTarget func(string) error) (bool, string) {
	evidence := request.FileApproval
	if evidence == nil {
		return false, "未收到同一 item/started 的完整文件变更清单"
	}
	wanted, grantRoot, err := fileApprovalKeyFromRequest(request.Params, request.Generation)
	if err != nil || wanted != evidence.Key {
		return false, "文件变更清单与审批请求不匹配"
	}
	if evidence.CapturedAt.IsZero() || now.Before(evidence.CapturedAt) || now.Sub(evidence.CapturedAt) >= fileApprovalTTL {
		return false, "文件变更清单已过期"
	}
	if checkTarget == nil {
		return false, "接收端未配置文件授权检查"
	}
	if evidence.CWD != "" && (checkWorkspace == nil || checkedApprovalPath(evidence.CWD, "", checkWorkspace) != nil) {
		return false, "文件变更工作目录超出允许范围"
	}
	canonical, err := json.Marshal(evidence.Changes)
	if err != nil || len(evidence.Changes) == 0 || len(evidence.ChangesJSON) == 0 ||
		sha256.Sum256(canonical) != evidence.Digest || sha256.Sum256(evidence.ChangesJSON) != evidence.Digest {
		return false, "文件变更清单不完整"
	}
	for _, change := range evidence.Changes {
		if err := checkedApprovalPath(change.Path, evidence.CWD, checkTarget); err != nil {
			return false, "文件变更超出允许目录"
		}
		if change.Kind.MovePath != nil {
			if err := checkedApprovalPath(*change.Kind.MovePath, evidence.CWD, checkTarget); err != nil {
				return false, "文件移动路径超出允许目录"
			}
		}
	}
	if grantRoot != nil && *grantRoot != "" {
		if err := checkedApprovalPath(*grantRoot, evidence.CWD, checkTarget); err != nil {
			return false, "请求的写入范围超出允许目录"
		}
	}
	return true, ""
}

func sameFileApprovalEvidence(left, right *fileApprovalEvidence) bool {
	return left != nil && right != nil && left.Key == right.Key && left.CWD == right.CWD && left.Digest == right.Digest &&
		left.CapturedAt.Equal(right.CapturedAt)
}

func (c *Client) putFileApprovalLocked(evidence *fileApprovalEvidence) {
	if evidence == nil || evidence.Key.Generation != c.generation || evidence.Bytes <= 0 || evidence.Bytes > maxFileApprovalEntryBytes {
		return
	}
	now := time.Now()
	c.pruneFileApprovalsLocked(now)
	if c.fileApprovals == nil {
		c.fileApprovals = make(map[fileApprovalKey]*fileApprovalEvidence)
	}
	if old := c.fileApprovals[evidence.Key]; old != nil {
		c.fileApprovalBytes -= old.Bytes
		c.removeFileApprovalOrderLocked(evidence.Key)
	}
	for len(c.fileApprovals) >= maxFileApprovalEntries || c.fileApprovalBytes+evidence.Bytes > maxFileApprovalBytes {
		if len(c.fileApprovalOrder) == 0 {
			return
		}
		c.deleteFileApprovalLocked(c.fileApprovalOrder[0])
	}
	c.fileApprovals[evidence.Key] = evidence
	c.fileApprovalOrder = append(c.fileApprovalOrder, evidence.Key)
	c.fileApprovalBytes += evidence.Bytes
}

func (c *Client) getFileApprovalLocked(key fileApprovalKey, now time.Time) *fileApprovalEvidence {
	c.pruneFileApprovalsLocked(now)
	return c.fileApprovals[key]
}

func (c *Client) pruneFileApprovalsLocked(now time.Time) {
	for _, key := range append([]fileApprovalKey(nil), c.fileApprovalOrder...) {
		evidence := c.fileApprovals[key]
		if evidence == nil || evidence.CapturedAt.IsZero() || now.Before(evidence.CapturedAt) || now.Sub(evidence.CapturedAt) >= fileApprovalTTL {
			c.deleteFileApprovalLocked(key)
		}
	}
}

func (c *Client) deleteFileApprovalLocked(key fileApprovalKey) {
	if evidence := c.fileApprovals[key]; evidence != nil {
		c.fileApprovalBytes -= evidence.Bytes
		if c.fileApprovalBytes < 0 {
			c.fileApprovalBytes = 0
		}
		delete(c.fileApprovals, key)
	}
	c.removeFileApprovalOrderLocked(key)
}

func (c *Client) removeFileApprovalOrderLocked(key fileApprovalKey) {
	for index, ordered := range c.fileApprovalOrder {
		if ordered != key {
			continue
		}
		copy(c.fileApprovalOrder[index:], c.fileApprovalOrder[index+1:])
		c.fileApprovalOrder = c.fileApprovalOrder[:len(c.fileApprovalOrder)-1]
		return
	}
}

func (c *Client) deleteFileApprovalsForTurnLocked(generation uint64, threadID, turnID string) {
	for key := range c.fileApprovals {
		if key.Generation == generation && key.ThreadID == threadID && key.TurnID == turnID {
			c.deleteFileApprovalLocked(key)
		}
	}
}

func (c *Client) deleteFileApprovalsForThreadLocked(threadID string) {
	for key := range c.fileApprovals {
		if key.ThreadID == threadID {
			c.deleteFileApprovalLocked(key)
		}
	}
}

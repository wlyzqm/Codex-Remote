package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

var submissionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9-]{16,80}$`)

type submissionRecord struct {
	Fingerprint string          `json:"fingerprint"`
	Status      int             `json:"status"`
	Response    json.RawMessage `json:"response,omitempty"`
}

func (s *Server) submissionPath(id string) string {
	return filepath.Join(s.cfg.UploadRoot, ".submissions", id+".json")
}

// A policy rejection is safe to correct only if this ID has never reached Codex.
// An existing record may represent a previous send whose reply was lost.
func (s *Server) rejectSubmission(w http.ResponseWriter, id, code, message string) {
	s.submissionMu.Lock()
	defer s.submissionMu.Unlock()
	if submissionIDPattern.MatchString(id) {
		if _, err := os.Stat(s.submissionPath(id)); errors.Is(err, os.ErrNotExist) {
			code = "submission_not_sent"
		}
	}
	writeError(w, http.StatusForbidden, code, message)
}

// beginSubmission reserves an ID before sending anything to Codex. A pending
// record left by a restart is uncertain and must never execute a second time.
func (s *Server) beginSubmission(w http.ResponseWriter, id, method string, params json.RawMessage) bool {
	if !submissionIDPattern.MatchString(id) || (method != "thread/start" && method != "turn/start" && method != "turn/steer") {
		writeError(w, http.StatusBadRequest, "invalid_submission", "提交标识或方法无效")
		return false
	}
	hash := sha256.Sum256(append([]byte(method+"\n"), params...))
	fingerprint := hex.EncodeToString(hash[:])
	s.submissionMu.Lock()
	defer s.submissionMu.Unlock()
	path := s.submissionPath(id)
	data, err := os.ReadFile(path)
	if err == nil {
		var record submissionRecord
		if json.Unmarshal(data, &record) != nil {
			writeError(w, http.StatusConflict, "submission_unknown", "提交记录不完整，请先核对会话；不会重复执行")
		} else if record.Fingerprint != fingerprint && record.Fingerprint != legacySubmissionFingerprint(method, params) {
			writeError(w, http.StatusConflict, "submission_mismatch", "提交标识已用于另一条指令")
		} else if record.Status != 0 {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(record.Status)
			_, _ = w.Write(record.Response)
		} else if s.submissions[id] {
			writeError(w, http.StatusConflict, "submission_pending", "工作站仍在确认这次提交，请稍后核对")
		} else {
			writeError(w, http.StatusConflict, "submission_unknown", "工作站无法确认上次提交结果，请先检查会话；不会重复执行")
		}
		return false
	}
	if !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusServiceUnavailable, "submission_unavailable", "无法读取提交记录，尚未发送")
		return false
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		data, _ = json.Marshal(submissionRecord{Fingerprint: fingerprint})
		var file *os.File
		file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = file.Write(data)
			if err == nil {
				err = file.Sync()
			}
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
		}
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "submission_unavailable", "无法保存提交记录，尚未发送")
		return false
	}
	s.submissions[id] = true
	return true
}

func (s *Server) completeSubmission(w http.ResponseWriter, id string, status int, response json.RawMessage) bool {
	s.submissionMu.Lock()
	defer s.submissionMu.Unlock()
	path := s.submissionPath(id)
	data, err := os.ReadFile(path)
	var record submissionRecord
	if err == nil {
		err = json.Unmarshal(data, &record)
	}
	if err == nil {
		record.Status, record.Response = status, response
		data, err = json.Marshal(record)
	}
	if err == nil {
		var file *os.File
		file, err = os.CreateTemp(filepath.Dir(path), ".result-")
		if err == nil {
			defer os.Remove(file.Name())
			_, err = file.Write(data)
			if err == nil {
				err = file.Sync()
			}
			if closeErr := file.Close(); err == nil {
				err = closeErr
			}
			if err == nil {
				err = os.Rename(file.Name(), path)
			}
		}
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "submission_unknown", "指令可能已被接收，但结果未能保存，请先核对会话")
		return false
	}
	// ponytail: retain one small record per submission; pruning requires retired-ID
	// tombstones so an old browser retry cannot execute the same command again.
	return true
}

// 0.4.0 included receiver-owned approval defaults in the fingerprint. Preserve
// those acknowledgements when upgrading the workstation's access settings.
func legacySubmissionFingerprint(method string, params json.RawMessage) string {
	var legacy map[string]any
	if json.Unmarshal(params, &legacy) != nil {
		return ""
	}
	if method == "turn/start" {
		delete(legacy, "approvalPolicy")
		delete(legacy, "approvalsReviewer")
		delete(legacy, "sandboxPolicy")
	}
	if method == "thread/start" {
		legacy["approvalPolicy"] = "on-request"
		legacy["approvalsReviewer"] = "auto_review"
		legacy["sandbox"] = "workspace-write"
	}
	raw, _ := json.Marshal(legacy)
	hash := sha256.Sum256(append([]byte(method+"\n"), raw...))
	return hex.EncodeToString(hash[:])
}

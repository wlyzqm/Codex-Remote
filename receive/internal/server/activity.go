package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type requestHistory struct {
	Key      string    `json:"key"`
	Method   string    `json:"method"`
	ThreadID string    `json:"threadId"`
	Summary  string    `json:"summary"`
	Status   string    `json:"status"`
	Decision string    `json:"decision,omitempty"`
	At       time.Time `json:"at"`
}
type activityStore struct {
	sync.Mutex `json:"-"`
	History    []requestHistory `json:"history"`
}

func (s *Server) activityPath() string { return filepath.Join(s.cfg.UploadRoot, ".remote-state.json") }
func (s *Server) loadActivity() error {
	s.activity = &activityStore{}
	data, err := os.ReadFile(s.activityPath())
	if err == nil {
		if err = json.Unmarshal(data, s.activity); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for i := range s.activity.History {
		if s.activity.History[i].Status == "pending" {
			s.activity.History[i].Status = "unknown"
		}
	}
	return nil
}

// Small bounded workstation history; keep the previous file intact on failure.
func (s *Server) saveActivityLocked() error {
	data, err := json.Marshal(s.activity)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(s.cfg.UploadRoot, ".activity-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), s.activityPath())
}
func (s *Server) Observe(data []byte) {
	var event struct {
		Type, Method, Key, Reason string
		Connected                 bool
		Decision                  any
		Request                   struct {
			Key, Method string
			Params      struct{ ThreadID, ConversationID, Command, Reason, Message string }
		}
		Params struct {
			ThreadID, TurnID string
			Turn             struct{ ID, Status string }
		}
	}
	if json.Unmarshal(data, &event) != nil {
		return
	}
	a := s.activity
	a.Lock()
	changed := false
	switch event.Type {
	case "server_request":
		thread := event.Request.Params.ThreadID
		if thread == "" {
			thread = event.Request.Params.ConversationID
		}
		summary := event.Request.Params.Command
		if summary == "" {
			summary = event.Request.Params.Reason
		}
		if summary == "" {
			summary = event.Request.Params.Message
		}
		if len([]rune(summary)) > 240 {
			summary = string([]rune(summary)[:240])
		}
		a.History = append([]requestHistory{{Key: event.Request.Key, Method: event.Request.Method, ThreadID: thread, Summary: summary, Status: "pending", At: time.Now()}}, a.History...)
		if len(a.History) > 100 {
			a.History = a.History[:100]
		}
		changed = true
	case "server_request_answered":
		for i := range a.History {
			if a.History[i].Key == event.Key {
				a.History[i].Status = event.Reason
				if event.Reason == "" {
					a.History[i].Status = "answered"
				}
				if event.Decision != nil {
					decision := event.Decision
					if object, ok := decision.(map[string]any); ok {
						decision = object["decision"]
					}
					if value, ok := decision.(string); ok {
						a.History[i].Decision = map[string]string{"accept": "已允许", "approved": "已允许", "decline": "已拒绝", "cancel": "已取消", "abort": "已取消"}[value]
					}
				}
				a.History[i].At = time.Now()
				changed = true
				break
			}
		}
	case "backend_status":
		if !event.Connected {
			for i := range a.History {
				if a.History[i].Status == "pending" {
					a.History[i].Status = "disconnected"
					changed = true
				}
			}
		}

	}
	if changed {
		if err := s.saveActivityLocked(); err != nil {
			s.cfg.Logger.Printf("save workstation history: %v", err)
		}
	}
	a.Unlock()
}
func (s *Server) handleUploads(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		path, err := s.uploadedFileTarget(r.URL.Query().Get("path"))
		if err != nil || strings.HasPrefix(filepath.Base(path), ".") {
			writeError(w, 400, "invalid_upload", "附件不存在")
			return
		}
		if err = os.Remove(path); err != nil {
			writeError(w, 500, "delete_failed", err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	entries, err := os.ReadDir(s.cfg.UploadRoot)
	if err != nil {
		writeError(w, 500, "uploads_unavailable", err.Error())
		return
	}
	files := []map[string]any{}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		name := entry.Name()
		if len(name) > 33 && name[32] == '-' {
			name = name[33:]
		}
		files = append(files, map[string]any{"name": name, "path": filepath.Join(s.cfg.UploadRoot, entry.Name()), "size": info.Size(), "at": info.ModTime()})
	}
	sort.Slice(files, func(i, j int) bool { return files[i]["at"].(time.Time).After(files[j]["at"].(time.Time)) })
	writeJSON(w, 200, map[string]any{"data": files})
}

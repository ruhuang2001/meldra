package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const sessionsDirName = "sessions"

type SessionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Session struct {
	ID                 string           `json:"id"`
	Workspace          string           `json:"workspace"`
	CreatedAt          time.Time        `json:"created_at"`
	UpdatedAt          time.Time        `json:"updated_at"`
	PreviousResponseID string           `json:"previous_response_id,omitempty"`
	Messages           []SessionMessage `json:"messages,omitempty"`
	Plan               []string         `json:"plan,omitempty"`
	Summary            string           `json:"summary,omitempty"`
	resumed            bool
}

type SessionStore struct {
	paths ConfigPaths
	dir   string
}

func NewSessionStore(paths ConfigPaths) *SessionStore {
	return &SessionStore{paths: paths, dir: filepath.Join(paths.Home, sessionsDirName)}
}

func (s *SessionStore) Create(workspace string) (*Session, error) {
	session, err := s.New(workspace)
	if err != nil {
		return nil, err
	}
	if err := s.Save(session); err != nil {
		return nil, err
	}
	return session, nil
}

func (s *SessionStore) New(workspace string) (*Session, error) {
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	return &Session{ID: id, Workspace: workspace, CreatedAt: now, UpdatedAt: now}, nil
}

func newSessionID() (string, error) {
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create session ID: %w", err)
	}
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(random), nil
}

func (s *SessionStore) Save(session *Session) error {
	if session == nil || !validSessionID(session.ID) {
		return fmt.Errorf("invalid session")
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	session.UpdatedAt = time.Now().UTC()
	if len(session.Messages) > 100 {
		session.Messages = append([]SessionMessage(nil), session.Messages[len(session.Messages)-100:]...)
	}
	contents, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	contents = append(contents, '\n')
	target := s.path(session.ID)
	temp, err := os.CreateTemp(s.dir, ".session-*.tmp")
	if err != nil {
		return fmt.Errorf("create session file: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(privateFilePerm); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(contents); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return os.Chmod(target, privateFilePerm)
}

func (s *SessionStore) Load(id string) (*Session, error) {
	if id == "latest" {
		sessions, err := s.List()
		if err != nil {
			return nil, err
		}
		if len(sessions) == 0 {
			return nil, fmt.Errorf("no saved sessions")
		}
		id = sessions[0].ID
	}
	if !validSessionID(id) {
		return nil, fmt.Errorf("invalid session ID %q", id)
	}
	contents, err := readSessionFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("session %q not found", id)
	}
	if err != nil {
		return nil, err
	}
	var session Session
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&session); err != nil {
		return nil, fmt.Errorf("parse session %q: %w", id, err)
	}
	if session.ID != id || session.Workspace == "" {
		return nil, fmt.Errorf("session %q is invalid", id)
	}
	session.resumed = true
	return &session, nil
}

func (s *SessionStore) Delete(id string) error {
	if !validSessionID(id) {
		return fmt.Errorf("invalid session ID %q", id)
	}
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete session %q: %w", id, err)
	}
	return nil
}

func (s *SessionStore) List() ([]Session, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	var sessions []Session
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validSessionID(id) {
			continue
		}
		session, err := s.Load(id)
		if err != nil {
			continue
		}
		if len(session.Messages) == 0 {
			_ = s.Delete(id)
			continue
		}
		workspaceInfo, err := os.Stat(session.Workspace)
		if errors.Is(err, fs.ErrNotExist) || (err == nil && !workspaceInfo.IsDir()) {
			_ = s.Delete(id)
			continue
		}
		if err != nil {
			continue
		}
		sessions = append(sessions, *session)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt) })
	return sessions, nil
}

func (s *SessionStore) ListWorkspace(workspace string) ([]Session, error) {
	root, err := canonicalWorkspacePath(workspace)
	if err != nil {
		return nil, err
	}
	sessions, err := s.List()
	if err != nil {
		return nil, err
	}
	filtered := make([]Session, 0, len(sessions))
	for _, session := range sessions {
		sessionRoot, err := canonicalWorkspacePath(session.Workspace)
		if err == nil && sessionRoot == root {
			filtered = append(filtered, session)
		}
	}
	return filtered, nil
}

func canonicalWorkspacePath(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace %q: %w", workspace, err)
	}
	return filepath.Clean(canonical), nil
}

func (s *SessionStore) ensureDir() error {
	if err := ensureConfigHome(s.paths); err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, privateDirPerm); err != nil {
		return fmt.Errorf("create sessions directory: %w", err)
	}
	info, err := os.Lstat(s.dir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("sessions path must be a directory, not a symbolic link")
	}
	return os.Chmod(s.dir, privateDirPerm)
}

func (s *SessionStore) path(id string) string {
	return filepath.Join(s.dir, id+".json")
}

func readSessionFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("session file must be a regular file, not a symbolic link")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("session file permissions are %04o; expected 0600", info.Mode().Perm())
	}
	return os.ReadFile(path)
}

func validSessionID(id string) bool {
	if id == "" {
		return false
	}
	for _, character := range id {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func (s *Session) appendMessage(role, content string) {
	if content == "" {
		return
	}
	if len(content) > 16<<10 {
		content = content[:16<<10] + "\n[truncated]"
	}
	s.Messages = append(s.Messages, SessionMessage{Role: role, Content: content})
}

func (s *Session) resumeContext() string {
	var result strings.Builder
	if s.Summary != "" {
		fmt.Fprintf(&result, "Previous session summary:\n%s\n\n", s.Summary)
	}
	if len(s.Plan) > 0 {
		result.WriteString("Current plan:\n")
		for index, step := range s.Plan {
			fmt.Fprintf(&result, "%d. %s\n", index+1, step)
		}
		result.WriteString("\n")
	}
	start := len(s.Messages) - 12
	if start < 0 {
		start = 0
	}
	if start < len(s.Messages) {
		result.WriteString("Recent conversation:\n")
		for _, message := range s.Messages[start:] {
			fmt.Fprintf(&result, "%s: %s\n", message.Role, message.Content)
		}
	}
	return result.String()
}

type SessionTools struct {
	session *Session
	store   *SessionStore
}

func NewSessionTools(session *Session, store *SessionStore) *SessionTools {
	return &SessionTools{session: session, store: store}
}

func (s *SessionTools) ToolDefinitions() []ToolDefinition {
	return []ToolDefinition{
		{
			Name:        "update_plan",
			Description: "Replace the saved implementation plan for this session with a concise ordered list.",
			Parameters: objectSchema(map[string]any{
				"steps": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": 20},
			}, []string{"steps"}),
			Function: s.updatePlan,
		},
		{
			Name:        "save_summary",
			Description: "Save a concise session summary containing completed work, verification, and remaining work so the session can be resumed later.",
			Parameters: objectSchema(map[string]any{
				"summary": map[string]any{"type": "string"},
			}, []string{"summary"}),
			Function: s.saveSummary,
		},
		{
			Name:        "session_status",
			Description: "Return this session's ID, workspace, saved plan, and summary.",
			Parameters:  objectSchema(map[string]any{}, nil),
			Function:    s.status,
		},
	}
}

func (s *SessionTools) updatePlan(raw json.RawMessage) (string, error) {
	var input struct {
		Steps []string `json:"steps"`
	}
	if err := decodeToolInput(raw, &input, "steps"); err != nil {
		return "", err
	}
	if len(input.Steps) > 20 {
		return "", fmt.Errorf("plan may contain at most 20 steps")
	}
	for index := range input.Steps {
		input.Steps[index] = strings.TrimSpace(input.Steps[index])
		if input.Steps[index] == "" {
			return "", fmt.Errorf("plan steps must not be empty")
		}
	}
	s.session.Plan = append([]string(nil), input.Steps...)
	if err := s.store.Save(s.session); err != nil {
		return "", err
	}
	return s.status(json.RawMessage(`{}`))
}

func (s *SessionTools) saveSummary(raw json.RawMessage) (string, error) {
	var input struct {
		Summary string `json:"summary"`
	}
	if err := decodeToolInput(raw, &input, "summary"); err != nil {
		return "", err
	}
	input.Summary = strings.TrimSpace(input.Summary)
	if input.Summary == "" {
		return "", fmt.Errorf("summary must not be empty")
	}
	if len(input.Summary) > 32<<10 {
		return "", fmt.Errorf("summary is too large")
	}
	s.session.Summary = input.Summary
	if err := s.store.Save(s.session); err != nil {
		return "", err
	}
	return "Session summary saved.", nil
}

func (s *SessionTools) status(raw json.RawMessage) (string, error) {
	var input struct{}
	if err := decodeToolInput(raw, &input); err != nil {
		return "", err
	}
	contents, err := json.MarshalIndent(map[string]any{
		"id":        s.session.ID,
		"workspace": s.session.Workspace,
		"plan":      s.session.Plan,
		"summary":   s.session.Summary,
	}, "", "  ")
	return string(contents), err
}

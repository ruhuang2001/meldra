package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	sessionsDirName                = "sessions"
	maxSessionMessageBytes         = 16 << 10
	maxSessionFileBytes            = 2 << 20
	maxSessionMessages             = 100
	maxSessionIDBytes              = 256
	maxSessionPathBytes            = 32 << 10
	maxSessionProviderIDBytes      = 32 << 10
	maxSessionPlanSteps            = 20
	maxSessionPlanStepBytes        = 32 << 10
	maxSessionSummaryBytes         = 32 << 10
	sessionMessageTruncationSuffix = "\n[truncated]"
)

type SessionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Session struct {
	ID                   string           `json:"id"`
	Workspace            string           `json:"workspace"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
	PreviousResponseID   string           `json:"previous_response_id,omitempty"`
	Messages             []SessionMessage `json:"messages,omitempty"`
	Plan                 []string         `json:"plan,omitempty"`
	Summary              string           `json:"summary,omitempty"`
	WorkspaceUnavailable bool             `json:"-"`
	resumed              bool
	savedRevision        [sha256.Size]byte
	hasSavedRevision     bool
}

type SessionStore struct {
	paths ConfigPaths
	dir   string
}

// SessionListDiagnostics describes files skipped while listing sessions.
// It intentionally omits file names and errors because session files can
// contain user prompts, summaries, and provider response IDs.
type SessionListDiagnostics struct {
	SkippedFiles int
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
	random := make([]byte, 16)
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
	unlock, err := acquireSessionStoreLock(s.dir)
	if err != nil {
		return err
	}
	defer unlock()
	session.UpdatedAt = time.Now().UTC()
	if len(session.Messages) > maxSessionMessages {
		session.Messages = append([]SessionMessage(nil), session.Messages[len(session.Messages)-maxSessionMessages:]...)
	}
	if err := validateSession(session, session.ID); err != nil {
		return fmt.Errorf("invalid session: %w", err)
	}
	contents, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	contents = append(contents, '\n')
	if len(contents) > maxSessionFileBytes {
		return fmt.Errorf("session file exceeds %d byte limit", maxSessionFileBytes)
	}
	target := s.path(session.ID)
	if session.hasSavedRevision {
		current, err := readSessionFile(target)
		if err != nil {
			return fmt.Errorf("check session before saving: %w", err)
		}
		if sha256.Sum256(current) != session.savedRevision {
			return fmt.Errorf("session %q has changed since it was loaded or last saved; refusing to overwrite newer state", session.ID)
		}
	} else if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("session %q already exists; refusing to overwrite it", session.ID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("check session before saving: %w", err)
	}
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
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("sync session file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	if err := syncDirectory(s.dir); err != nil {
		return err
	}
	session.savedRevision = sha256.Sum256(contents)
	session.hasSavedRevision = true
	return nil
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
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&session); err != nil {
		return nil, fmt.Errorf("parse session %q: %w", id, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("parse session %q: trailing data", id)
		}
		return nil, fmt.Errorf("parse session %q: trailing data: %w", id, err)
	}
	if err := validateSession(&session, id); err != nil {
		return nil, fmt.Errorf("session %q is invalid", id)
	}
	session.resumed = true
	session.savedRevision = sha256.Sum256(contents)
	session.hasSavedRevision = true
	return &session, nil
}

func (s *SessionStore) Delete(id string) error {
	if !validSessionID(id) {
		return fmt.Errorf("invalid session ID %q", id)
	}
	if err := os.Remove(s.path(id)); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("delete session %q: %w", id, err)
		}
		return nil
	}
	if err := syncDirectory(s.dir); err != nil {
		return fmt.Errorf("sync sessions directory: %w", err)
	}
	return nil
}

func (s *SessionStore) List() ([]Session, error) {
	sessions, _, err := s.ListWithDiagnostics()
	return sessions, err
}

// ListWithDiagnostics returns saved sessions along with a safe aggregate
// diagnostic for unreadable or invalid session files. Like List, it never
// modifies session files.
func (s *SessionStore) ListWithDiagnostics() ([]Session, SessionListDiagnostics, error) {
	var diagnostics SessionListDiagnostics
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, diagnostics, nil
	}
	if err != nil {
		return nil, diagnostics, fmt.Errorf("list sessions: %w", err)
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
		session, messageCount, err := readSessionListMetadata(s.path(id), id)
		if err != nil {
			diagnostics.SkippedFiles++
			continue
		}
		if messageCount == 0 {
			continue
		}
		workspaceInfo, err := os.Stat(session.Workspace)
		session.WorkspaceUnavailable = err != nil || !workspaceInfo.IsDir()
		sessions = append(sessions, *session)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt) })
	return sessions, diagnostics, nil
}

func (s *SessionStore) ListWorkspace(workspace string) ([]Session, error) {
	sessions, _, err := s.ListWorkspaceWithDiagnostics(workspace)
	return sessions, err
}

// ListWorkspaceWithDiagnostics returns the sessions that belong to workspace
// along with aggregate diagnostics for any skipped session files.
func (s *SessionStore) ListWorkspaceWithDiagnostics(workspace string) ([]Session, SessionListDiagnostics, error) {
	root, err := canonicalWorkspacePath(workspace)
	if err != nil {
		return nil, SessionListDiagnostics{}, err
	}
	sessions, diagnostics, err := s.ListWithDiagnostics()
	if err != nil {
		return nil, diagnostics, err
	}
	filtered := make([]Session, 0, len(sessions))
	for _, session := range sessions {
		sessionRoot, err := canonicalWorkspacePath(session.Workspace)
		if err == nil && sessionRoot == root {
			filtered = append(filtered, session)
		}
	}
	return filtered, diagnostics, nil
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
	if err := makeDirectoryTreeDurable(s.dir, privateDirPerm, syncDirectory); err != nil {
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
	if info.Size() > maxSessionFileBytes {
		return nil, fmt.Errorf("session file exceeds %d byte limit", maxSessionFileBytes)
	}
	return os.ReadFile(path)
}

func validateSession(session *Session, expectedID string) error {
	if session.ID != expectedID || len(session.ID) > maxSessionIDBytes || session.Workspace == "" || len(session.Workspace) > maxSessionPathBytes {
		return fmt.Errorf("invalid identity or workspace")
	}
	if len(session.PreviousResponseID) > maxSessionProviderIDBytes || len(session.Summary) > maxSessionSummaryBytes {
		return fmt.Errorf("oversized metadata")
	}
	if len(session.Messages) > maxSessionMessages || len(session.Plan) > maxSessionPlanSteps {
		return fmt.Errorf("too many messages or plan steps")
	}
	for _, message := range session.Messages {
		if (message.Role != "user" && message.Role != "assistant") || len(message.Content) > maxSessionMessageBytes {
			return fmt.Errorf("invalid message")
		}
	}
	for _, step := range session.Plan {
		if len(step) > maxSessionPlanStepBytes {
			return fmt.Errorf("oversized plan step")
		}
	}
	return nil
}

// readSessionListMetadata validates the complete schema while retaining only
// the message needed to produce the existing latest-user list preview.
func readSessionListMetadata(path, expectedID string) (*Session, int, error) {
	contents, err := readSessionFile(path)
	if err != nil {
		return nil, 0, err
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	var session Session
	var messageCount int
	var latestUser, latestAssistant SessionMessage
	var latestUserIndex, latestAssistantIndex int
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, 0, fmt.Errorf("parse session metadata")
	}
	seen := make(map[string]bool, 8)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, 0, err
		}
		key, ok := keyToken.(string)
		if !ok || seen[key] {
			return nil, 0, fmt.Errorf("invalid or duplicate session field")
		}
		seen[key] = true
		switch key {
		case "id":
			err = decoder.Decode(&session.ID)
		case "workspace":
			err = decoder.Decode(&session.Workspace)
		case "created_at":
			err = decoder.Decode(&session.CreatedAt)
		case "updated_at":
			err = decoder.Decode(&session.UpdatedAt)
		case "previous_response_id":
			err = decoder.Decode(&session.PreviousResponseID)
		case "plan":
			err = decoder.Decode(&session.Plan)
		case "summary":
			err = decoder.Decode(&session.Summary)
		case "messages":
			var start json.Token
			start, err = decoder.Token()
			if err == nil && start == nil {
				break
			}
			if err == nil && start != json.Delim('[') {
				err = fmt.Errorf("messages must be an array")
			}
			for err == nil && decoder.More() {
				var message SessionMessage
				err = decoder.Decode(&message)
				messageCount++
				if err == nil && ((message.Role != "user" && message.Role != "assistant") || len(message.Content) > maxSessionMessageBytes || messageCount > maxSessionMessages) {
					err = fmt.Errorf("invalid message")
				}
				if err == nil && message.Role == "user" {
					latestUser, latestUserIndex = message, messageCount
				} else if err == nil {
					latestAssistant, latestAssistantIndex = message, messageCount
				}
			}
			if err == nil {
				_, err = decoder.Token()
			}
		default:
			err = fmt.Errorf("unknown session field %q", key)
		}
		if err != nil {
			return nil, 0, err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return nil, 0, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, 0, fmt.Errorf("trailing session data")
	}
	if err := validateSession(&session, expectedID); err != nil {
		return nil, 0, err
	}
	if latestUserIndex > 0 && latestAssistantIndex > 0 {
		if latestUserIndex < latestAssistantIndex {
			session.Messages = []SessionMessage{latestUser, latestAssistant}
		} else {
			session.Messages = []SessionMessage{latestAssistant, latestUser}
		}
	} else if latestUserIndex > 0 {
		session.Messages = []SessionMessage{latestUser}
	} else if latestAssistantIndex > 0 {
		session.Messages = []SessionMessage{latestAssistant}
	}
	return &session, messageCount, nil
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
	content = truncateSessionMessage(content)
	s.Messages = append(s.Messages, SessionMessage{Role: role, Content: content})
}

func truncateSessionMessage(content string) string {
	return truncateUTF8(content, maxSessionMessageBytes, sessionMessageTruncationSuffix)
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
		if len(input.Steps[index]) > maxSessionPlanStepBytes {
			return "", fmt.Errorf("plan steps must not exceed %d bytes", maxSessionPlanStepBytes)
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
	if len(input.Summary) > maxSessionSummaryBytes {
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

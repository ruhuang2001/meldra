package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	maxReadLines            = 1000
	maxSearchHits           = 500
	maxToolOutput           = 256 << 10
	maxUndoSteps            = 10
	maxUndoEntryBytes       = 8 << 20
	maxLineBytes            = 1 << 20
	maxSearchBytes          = 8 << 20
	maxSearchFiles          = 5000
	maxListEntries          = 10000
	maxWalkEntries          = 20000
	maxPatchBytes           = 1 << 20
	maxChangeFiles          = 32
	maxEditableFileBytes    = 4 << 20
	maxChangeContentBytes   = 8 << 20
	maxApprovalPreviewBytes = 1 << 20
	defaultTimeout          = 600
	maxCommandTimeout       = 86400
)

var errWalkBounded = errors.New("workspace walk bounded")

var ErrWorkspaceBusy = errors.New("workspace already has an active tool call")

type changeBudget struct {
	total int64
}

type Workspace struct {
	web                webRuntime
	mcpBindings        map[string]string
	mcpExpectedBinding map[string]string
	processes          *processManager
	commandTimeout     int
	commandOutput      func(CommandOutput)
	commandExecutables map[string]string
	policy             *RuntimePolicy
	projectContext     *ProjectContext
	toolMu             sync.Mutex
	mcpInteractionMu   sync.Mutex
	mcpActiveServer    atomic.Pointer[mcpInteraction]
	mcpInput           func(context.Context, string) (string, bool)
	root               string
	input              *bufio.Reader
	output             io.Writer
	autoApprove        bool
	approve            ApprovalFunc
	approvalRecord     func(context.Context, ApprovalRequest, bool) error
	approvalPending    func(context.Context, ApprovalRequest) error
	hooks              *hookRunner
	hookSurface        string
	hookNotify         func(string)
	approvalGrant      func() bool
	present            ApprovalPresenter
	ctx                context.Context
	undoHistory        [][]fileChange
	protected          []string
	syncDir            func(string) error
}

func (w *Workspace) SetContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	w.ctx = ctx
}

func (w *Workspace) SetApprovalFunc(approve ApprovalFunc) {
	w.approve = approve
}

func (w *Workspace) SetApprovalPresenter(present ApprovalPresenter) {
	w.present = present
}

func (w *Workspace) contextErr() error {
	if w.ctx == nil {
		return nil
	}
	return w.ctx.Err()
}

func (w *Workspace) syncDirectory(path string) error {
	if w.syncDir != nil {
		return w.syncDir(path)
	}
	return syncDirectory(path)
}

func (w *Workspace) ProtectPath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if os.IsNotExist(err) {
		canonical = filepath.Clean(abs)
	} else if err != nil {
		return err
	}
	canonical = filepath.Clean(canonical)
	if canonical == w.root {
		return fmt.Errorf("workspace must not be the Meldra configuration directory")
	}
	w.protected = append(w.protected, canonical)
	return nil
}

func NewWorkspace(root string, input *bufio.Reader, output io.Writer, autoApprove bool) (*Workspace, error) {
	if input == nil || output == nil {
		return nil, fmt.Errorf("input and output are required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace root is not a directory")
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace root: %w", err)
	}
	if strings.EqualFold(filepath.Base(canonical), ".git") {
		return nil, fmt.Errorf("workspace root must not be .git")
	}
	return &Workspace{root: filepath.Clean(canonical), input: input, output: output, autoApprove: autoApprove, ctx: context.Background()}, nil
}

func (w *Workspace) ToolDefinitions() []ToolDefinition {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	nullableString := func(desc string) map[string]any {
		return map[string]any{"type": []string{"string", "null"}, "description": desc}
	}
	nullableInteger := func(desc string) map[string]any {
		return map[string]any{"type": []string{"integer", "null"}, "description": desc}
	}
	definitions := []ToolDefinition{
		{Name: "read_file", Description: "Read a file inside the workspace (never .git), with numbered, bounded line chunks.", Parameters: objectSchema(map[string]any{"path": str("Workspace-relative path, or an absolute path inside the workspace."), "offset": nullableInteger("Optional 1-based starting line; use null for 1."), "limit": nullableInteger("Optional line count; use null for 200 and values are capped.")}, []string{"path", "offset", "limit"}), Function: w.bindTool(w.readFile)},
		{Name: "list_files", Description: "Recursively list relative paths inside the workspace without following symlink directories or entering .git.", Parameters: objectSchema(map[string]any{"path": nullableString("Directory inside the workspace; use null for workspace root.")}, []string{"path"}), Function: w.bindTool(w.listFiles)},
		{Name: "search_files", Description: "Search regular files inside the workspace. Results and output are bounded; .git and symlink directories are skipped.", Parameters: objectSchema(map[string]any{"query": str("Literal text or RE2 regular expression to find."), "regex": map[string]any{"type": []string{"boolean", "null"}, "description": "Use RE2 regular expressions; null or false preserves literal matching."}, "path": nullableString("Directory or file inside the workspace; use null for workspace root."), "glob": nullableString("Optional filepath.Match pattern; use null for all files."), "max_results": nullableInteger("Optional result limit; use null for 100, capped at 500.")}, []string{"query", "path", "glob", "max_results", "regex"}), Function: w.bindTool(w.searchFiles)},
		{Name: "edit_file", Description: "Replace exact text once, or create a missing file with empty old_str. Prints a unified diff and requires [y/N] confirmation unless auto-approved. Paths must stay in the workspace and outside .git.", Parameters: objectSchema(map[string]any{"path": str("Target path inside workspace."), "old_str": str("Exact text occurring once; empty only to create."), "new_str": str("Replacement or new file contents.")}, []string{"path", "old_str", "new_str"}), Function: w.bindTool(w.editFile)},
		{Name: "apply_patch", Description: "Create, modify, or delete one or more files with a unified diff or atomic exact changes. Validates every path and hunk, prints the resulting diff, confirms before writing, and rolls back on failure. Set exactly one of patch or changes and set the other to null.", Parameters: objectSchema(map[string]any{"patch": nullableString("Unified diff with ---/+++/@@ hunks, including /dev/null for file creation or deletion and differing ---/+++ paths for a rename; or null."), "changes": map[string]any{"type": []string{"array", "null"}, "items": objectSchema(map[string]any{"path": str("Unique target path inside workspace."), "old_str": str("Exact text, or empty for creation."), "new_str": str("Replacement contents.")}, []string{"path", "old_str", "new_str"})}}, []string{"patch", "changes"}), Function: w.bindTool(w.applyPatch)},
		{Name: "undo_last_change", Description: "Undo the most recent successful workspace edit/apply_patch from this session after showing a reverse diff and confirming. Keeps up to 10 steps, newest first. Refuses if files changed since.", Parameters: objectSchema(map[string]any{}, nil), Function: w.bindTool(w.undo)},
		{Name: "run_command", Description: "Run an allowlisted command in the workspace without a shell (restricted Python, Go, npm/pnpm, Cargo, Make, formatting, or read-only Git commands). Commands that compile or execute workspace code require direct user confirmation unless auto-approved. Timeout defaults to the configured command deadline (600 seconds) and is capped at 86400 seconds.", Parameters: objectSchema(map[string]any{"command": str("Executable: python3, go, gofmt, npm, pnpm, cargo, make, or git."), "args": map[string]any{"type": []string{"array", "null"}, "items": str("One argument; no shell expansion.")}, "timeout": nullableInteger("Timeout seconds; null uses the configured default (600), maximum 86400.")}, []string{"command", "args", "timeout"}), Function: w.bindTool(w.runCommand)},
		{Name: "verify", Description: "Run a project-aware verification preset: test, check, build, format, or diff. Detects Make, Go, Python, Node, and Rust projects from root marker files. Presets that compile or execute workspace code require direct user confirmation unless auto-approved.", Parameters: objectSchema(map[string]any{"preset": map[string]any{"type": "string", "enum": []string{"test", "check", "build", "format", "diff"}}}, []string{"preset"}), Function: w.bindTool(w.verify)},
		{Name: "git_review", Description: "Return bounded git status, a --stat summary of staged and unstaged changes, the diffs themselves, and recent log for the workspace; errors clearly outside a git repository.", Parameters: objectSchema(map[string]any{}, nil), Function: w.bindTool(w.gitReview)},
		{Name: "git_commit", Description: "Commit explicitly selected files after reviewing their exact diff and confirming. Refuses existing staged changes. Hooks and signing are disabled; failures may leave selected files staged. Inspect git_review before retrying.", Parameters: objectSchema(map[string]any{"paths": map[string]any{"type": "array", "items": str("Explicit workspace-relative file path; no directories or globs."), "minItems": 1, "maxItems": maxChangeFiles}, "message": str("Commit message.")}, []string{"paths", "message"}), Function: w.bindTool(w.gitCommit)},
	}
	return w.guardedDefinitions(append(definitions, w.webToolDefinitions()...))
}

func (w *Workspace) resolve(name string, write bool) (string, error) {
	if name == "" {
		name = "."
	}
	p := name
	if !filepath.IsAbs(p) {
		p = filepath.Join(w.root, p)
	}
	p = filepath.Clean(p)
	if !within(w.root, p) {
		return "", fmt.Errorf("path escapes workspace")
	}
	for _, protected := range w.protected {
		if withinFold(protected, p) {
			return "", fmt.Errorf("access to Meldra configuration and sessions is forbidden")
		}
	}
	rel, _ := filepath.Rel(w.root, p)
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if strings.EqualFold(part, ".git") {
			return "", fmt.Errorf("access to .git is forbidden")
		}
	}
	current := w.root
	parts := strings.Split(rel, string(filepath.Separator))
	for index, part := range parts {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) && write {
			return p, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symbolic links are not allowed in workspace tool paths")
		}
		if index < len(parts)-1 && !info.IsDir() {
			return "", fmt.Errorf("path component is not a directory")
		}
	}
	return p, nil
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func withinFold(root, path string) bool {
	rootParts := strings.Split(filepath.Clean(root), string(filepath.Separator))
	pathParts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	if len(pathParts) < len(rootParts) {
		return false
	}
	for index := range rootParts {
		if !strings.EqualFold(rootParts[index], pathParts[index]) {
			return false
		}
	}
	return true
}

func capText(s string) string {
	return truncateUTF8Text(s, maxToolOutput, false)
}

func capTextWithFlag(s string) (string, bool) {
	capped := truncateUTF8Text(s, maxToolOutput, false)
	return capped, len(capped) < len(s)
}

const outputTruncationSuffix = "\n[output truncated]"

func truncateUTF8Text(text string, limit int, forceSuffix bool) string {
	if limit <= 0 {
		return ""
	}
	if !forceSuffix && len(text) <= limit {
		return truncateUTF8(text, limit, outputTruncationSuffix)
	}
	suffix := outputTruncationSuffix
	if len(suffix) > limit {
		if limit >= 3 {
			suffix = "..."
		} else {
			suffix = strings.Repeat(".", limit)
		}
	}
	return truncateUTF8WithSuffix(text, limit, suffix)
}

func (w *Workspace) bindTool(handler func(json.RawMessage) (string, error)) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, input json.RawMessage) (string, error) {
		if !w.toolMu.TryLock() {
			return "", ErrWorkspaceBusy
		}
		defer w.toolMu.Unlock()
		if err := ctx.Err(); err != nil {
			return "", err
		}
		previous := w.ctx
		w.ctx = ctx
		defer func() { w.ctx = previous }()
		return handler(input)
	}
}

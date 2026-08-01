package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxReadLines   = 1000
	maxSearchHits  = 500
	maxToolOutput  = 256 << 10
	maxLineBytes   = 1 << 20
	maxSearchBytes = 8 << 20
	maxSearchFiles = 5000
	maxListEntries = 10000
	maxWalkEntries = 20000
	defaultTimeout = 60
)

var errWalkBounded = errors.New("workspace walk bounded")

// Workspace owns the safe, workspace-scoped tool runtime and its in-memory undo state.
type Workspace struct {
	root        string
	input       *bufio.Reader
	output      io.Writer
	autoApprove bool
	approve     ApprovalFunc
	ctx         context.Context
	last        []fileChange
	protected   []string
}

func (w *Workspace) SetContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	w.ctx = ctx
}

// SetApprovalFunc replaces line-based confirmation with an interaction owned by
// the caller. A nil approval function preserves the line-based terminal prompt.
func (w *Workspace) SetApprovalFunc(approve ApprovalFunc) {
	w.approve = approve
}

func (w *Workspace) contextErr() error {
	if w.ctx == nil {
		return nil
	}
	return w.ctx.Err()
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

type fileChange struct {
	path                 string
	before, after        []byte
	existed, afterExists bool
	mode                 fs.FileMode
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
	return []ToolDefinition{
		{Name: "read_file", Description: "Read a file inside the workspace (never .git), with numbered, bounded line chunks.", Parameters: objectSchema(map[string]any{"path": str("Workspace-relative path, or an absolute path inside the workspace."), "offset": nullableInteger("Optional 1-based starting line; use null for 1."), "limit": nullableInteger("Optional line count; use null for 200 and values are capped.")}, []string{"path", "offset", "limit"}), Function: w.readFile},
		{Name: "list_files", Description: "Recursively list relative paths inside the workspace without following symlink directories or entering .git.", Parameters: objectSchema(map[string]any{"path": nullableString("Directory inside the workspace; use null for workspace root.")}, []string{"path"}), Function: w.listFiles},
		{Name: "search_files", Description: "Search regular files inside the workspace. Results and output are bounded; .git and symlink directories are skipped.", Parameters: objectSchema(map[string]any{"query": str("Literal text to find."), "path": nullableString("Directory or file inside the workspace; use null for workspace root."), "glob": nullableString("Optional filepath.Match pattern; use null for all files."), "max_results": nullableInteger("Optional result limit; use null for 100, capped at 500.")}, []string{"query", "path", "glob", "max_results"}), Function: w.searchFiles},
		{Name: "edit_file", Description: "Replace exact text once, or create a missing file with empty old_str. Prints a unified diff and requires [y/N] confirmation unless auto-approved. Paths must stay in the workspace and outside .git.", Parameters: objectSchema(map[string]any{"path": str("Target path inside workspace."), "old_str": str("Exact text occurring once; empty only to create."), "new_str": str("Replacement or new file contents.")}, []string{"path", "old_str", "new_str"}), Function: w.editFile},
		{Name: "apply_patch", Description: "Create, modify, or delete one or more files with a unified diff or atomic exact changes. Validates every path and hunk, prints the resulting diff, confirms before writing, and rolls back on failure. Set exactly one of patch or changes and set the other to null.", Parameters: objectSchema(map[string]any{"patch": nullableString("Unified diff with ---/+++/@@ hunks, including /dev/null for file creation or deletion; or null."), "changes": map[string]any{"type": []string{"array", "null"}, "items": objectSchema(map[string]any{"path": str("Unique target path inside workspace."), "old_str": str("Exact text, or empty for creation."), "new_str": str("Replacement contents.")}, []string{"path", "old_str", "new_str"})}}, []string{"patch", "changes"}), Function: w.applyPatch},
		{Name: "undo_last_change", Description: "Undo the last successful workspace edit/apply_patch from this session after showing a reverse diff and confirming. Refuses if files changed since.", Parameters: objectSchema(map[string]any{}, nil), Function: w.undo},
		{Name: "run_command", Description: "Run an allowlisted command in the workspace without a shell (a workspace-local Python file, workspace-local go run, go test/vet/build, gofmt -d, safe Make targets, or read-only git). Commands that compile or execute workspace code require direct user confirmation unless auto-approved. Timeout is capped at 120 seconds.", Parameters: objectSchema(map[string]any{"command": str("Executable: python3, go, gofmt, make, or git."), "args": map[string]any{"type": []string{"array", "null"}, "items": str("One argument; no shell expansion.")}, "timeout": nullableInteger("Timeout seconds; use null for 60, maximum 120.")}, []string{"command", "args", "timeout"}), Function: w.runCommand},
		{Name: "verify", Description: "Run a safe workspace verification preset: test, check, build, format, or diff. Presets that compile or execute workspace code require direct user confirmation unless auto-approved.", Parameters: objectSchema(map[string]any{"preset": map[string]any{"type": "string", "enum": []string{"test", "check", "build", "format", "diff"}}}, []string{"preset"}), Function: w.verify},
		{Name: "git_review", Description: "Return bounded git status, staged and unstaged diffs, and recent log for the workspace; errors clearly outside a git repository.", Parameters: objectSchema(map[string]any{}, nil), Function: w.gitReview},
	}
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

func (w *Workspace) readFile(raw json.RawMessage) (string, error) {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeToolInput(raw, &in, "path"); err != nil {
		return "", err
	}
	p, err := w.resolve(in.Path, false)
	if err != nil {
		return "", err
	}
	pathInfo, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	if !pathInfo.Mode().IsRegular() {
		return "", fmt.Errorf("read_file only supports regular files")
	}
	file, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("read_file only supports regular files")
	}
	off := in.Offset
	if off == 0 {
		off = 1
	}
	if off < 1 {
		return "", fmt.Errorf("offset must be at least 1")
	}
	limit := in.Limit
	if limit == 0 {
		limit = 200
	}
	if limit < 1 {
		return "", fmt.Errorf("limit must be positive")
	}
	if limit > maxReadLines {
		limit = maxReadLines
	}
	var out strings.Builder
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	lineNumber, shown, scannedBytes := 0, 0, 0
	more := false
	for scanner.Scan() {
		lineNumber++
		scannedBytes += len(scanner.Bytes()) + 1
		if scannedBytes > maxSearchBytes {
			more = true
			break
		}
		if lineNumber > off+limit-1 {
			more = true
			break
		}
		if lineNumber >= off {
			line := fmt.Sprintf("%6d\t%s\n", lineNumber, scanner.Text())
			if out.Len()+len(line) > maxToolOutput {
				more = true
				break
			}
			out.WriteString(line)
			shown++
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read file lines: %w", err)
	}
	start, end := 0, 0
	if shown > 0 {
		start, end = off, off+shown-1
	}
	fmt.Fprintf(&out, "[lines %d-%d; truncated=%t]", start, end, more)
	return out.String(), nil
}

func (w *Workspace) walk(start string, fn fs.WalkDirFunc) error {
	entry, err := os.Lstat(start)
	if err != nil {
		return err
	}
	rootEntry := fs.FileInfoToDirEntry(entry)
	if err := fn(start, rootEntry, nil); err != nil {
		return err
	}
	if !rootEntry.IsDir() {
		return nil
	}
	visited := 0
	var visit func(string) error
	visit = func(directory string) error {
		handle, err := os.Open(directory)
		if err != nil {
			return err
		}
		defer handle.Close()
		for {
			entries, readErr := handle.ReadDir(128)
			for _, entry := range entries {
				visited++
				if visited > maxWalkEntries {
					return errWalkBounded
				}
				path := filepath.Join(directory, entry.Name())
				if entry.IsDir() && strings.EqualFold(entry.Name(), ".git") {
					continue
				}
				protected := false
				for _, protectedPath := range w.protected {
					if entry.IsDir() && withinFold(protectedPath, path) {
						protected = true
						break
					}
				}
				if protected || entry.Type()&os.ModeSymlink != 0 {
					continue
				}
				if err := fn(path, entry, nil); err != nil {
					return err
				}
				if entry.IsDir() {
					if err := visit(path); err != nil {
						return err
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	return visit(start)
}

func (w *Workspace) listFiles(raw json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := decodeToolInput(raw, &in); err != nil {
		return "", err
	}
	p, err := w.resolve(in.Path, false)
	if err != nil {
		return "", err
	}
	var items []string
	truncated := false
	errListStop := errors.New("list bounded")
	err = w.walk(p, func(path string, d fs.DirEntry, _ error) error {
		if path == p {
			return nil
		}
		rel, _ := filepath.Rel(w.root, path)
		if d.IsDir() {
			rel += "/"
		}
		items = append(items, filepath.ToSlash(rel))
		if len(items) >= maxListEntries {
			truncated = true
			return errListStop
		}
		return nil
	})
	if errors.Is(err, errWalkBounded) {
		truncated = true
	} else if err != nil && !errors.Is(err, errListStop) {
		return "", err
	}
	if truncated {
		items = append(items, "[truncated]")
	}
	b, _ := json.Marshal(items)
	return capText(string(b)), nil
}

func (w *Workspace) searchFiles(raw json.RawMessage) (string, error) {
	var in struct {
		Query, Path, Glob string
		MaxResults        int `json:"max_results"`
	}
	if err := decodeToolInput(raw, &in, "query"); err != nil {
		return "", err
	}
	if in.Query == "" {
		return "", fmt.Errorf("query must not be empty")
	}
	p, err := w.resolve(in.Path, false)
	if err != nil {
		return "", err
	}
	max := in.MaxResults
	if max == 0 {
		max = 100
	}
	if max < 1 {
		return "", fmt.Errorf("max_results must be positive")
	}
	if max > maxSearchHits {
		max = maxSearchHits
	}
	var out strings.Builder
	hits := 0
	filesScanned := 0
	bytesScanned := int64(0)
	bounded := false
	errStop := errors.New("bounded")
	err = w.walk(p, func(path string, d fs.DirEntry, _ error) error {
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, _ := filepath.Rel(w.root, path)
		rel = filepath.ToSlash(rel)
		if in.Glob != "" {
			ok, e := filepath.Match(in.Glob, rel)
			if e == nil && !ok && !strings.Contains(in.Glob, "/") {
				ok, e = filepath.Match(in.Glob, filepath.Base(rel))
			}
			if e != nil {
				return e
			}
			if !ok {
				return nil
			}
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if info.Size() > maxSearchBytes {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		filesScanned++
		if filesScanned > maxSearchFiles {
			bounded = true
			return errStop
		}
		file, e := os.Open(path)
		if e != nil {
			return e
		}
		openedInfo, e := file.Stat()
		if e != nil {
			_ = file.Close()
			return e
		}
		if !openedInfo.Mode().IsRegular() {
			_ = file.Close()
			return nil
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
		lineNumber := 0
		stop := false
		binary := false
		for scanner.Scan() {
			lineNumber++
			line := scanner.Text()
			bytesScanned += int64(len(scanner.Bytes()) + 1)
			if bytesScanned > maxSearchBytes*4 {
				bounded = true
				stop = true
				break
			}
			if strings.ContainsRune(line, '\x00') {
				binary = true
				break
			}
			if strings.Contains(line, in.Query) {
				fmt.Fprintf(&out, "%s:%d:%s\n", rel, lineNumber, line)
				hits++
				if hits >= max || out.Len() >= maxToolOutput {
					bounded = true
					stop = true
					break
				}
			}
		}
		scanErr := scanner.Err()
		closeErr := file.Close()
		if scanErr != nil {
			return scanErr
		}
		if closeErr != nil {
			return closeErr
		}
		if binary {
			return nil
		}
		if stop {
			return errStop
		}
		return nil
	})
	if errors.Is(err, errWalkBounded) {
		bounded = true
	} else if err != nil && !errors.Is(err, errStop) {
		return "", err
	}
	if bounded {
		fmt.Fprintf(&out, "[truncated after %d results]\n", hits)
	}
	return capText(out.String()), nil
}

type changeInput struct {
	Path   string `json:"path"`
	OldStr string `json:"old_str"`
	NewStr string `json:"new_str"`
}

func (w *Workspace) editFile(raw json.RawMessage) (string, error) {
	var in struct {
		Path   string `json:"path"`
		OldStr string `json:"old_str"`
		NewStr string `json:"new_str"`
	}
	if err := decodeToolInput(raw, &in, "path", "old_str", "new_str"); err != nil {
		return "", err
	}
	return w.applyInputs([]changeInput{{in.Path, in.OldStr, in.NewStr}})
}

func (w *Workspace) applyPatch(raw json.RawMessage) (string, error) {
	var in struct {
		Patch   string        `json:"patch"`
		Changes []changeInput `json:"changes"`
	}
	if err := decodeToolInput(raw, &in); err != nil {
		return "", err
	}
	if (strings.TrimSpace(in.Patch) == "") == (len(in.Changes) == 0) {
		return "", fmt.Errorf("set exactly one of patch or changes")
	}
	if in.Patch != "" {
		changes, err := w.prepareUnifiedPatch(in.Patch)
		if err != nil {
			return "", err
		}
		return w.applyChanges(changes)
	}
	return w.applyInputs(in.Changes)
}

type patchHunk struct {
	oldStart, oldCount int
	newStart, newCount int
	lines              []string
}

type filePatch struct {
	oldPath, newPath string
	hunks            []patchHunk
}

var hunkHeaderPattern = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func parseUnifiedPatch(patch string) ([]filePatch, error) {
	lines := strings.Split(strings.ReplaceAll(patch, "\r\n", "\n"), "\n")
	var files []filePatch
	for index := 0; index < len(lines); {
		if lines[index] == "" {
			index++
			continue
		}
		if !strings.HasPrefix(lines[index], "--- ") || index+1 >= len(lines) || !strings.HasPrefix(lines[index+1], "+++ ") {
			return nil, fmt.Errorf("invalid unified diff near line %d: expected --- and +++ headers", index+1)
		}
		file := filePatch{oldPath: patchHeaderPath(lines[index][4:]), newPath: patchHeaderPath(lines[index+1][4:])}
		if file.oldPath == "" || file.newPath == "" || (file.oldPath == "/dev/null" && file.newPath == "/dev/null") {
			return nil, fmt.Errorf("invalid patch paths near line %d", index+1)
		}
		if file.oldPath != "/dev/null" && file.newPath != "/dev/null" && file.oldPath != file.newPath {
			return nil, fmt.Errorf("patch renames are not supported: %s -> %s", file.oldPath, file.newPath)
		}
		index += 2
		for index < len(lines) && !strings.HasPrefix(lines[index], "--- ") {
			if lines[index] == "" {
				index++
				continue
			}
			match := hunkHeaderPattern.FindStringSubmatch(lines[index])
			if match == nil {
				return nil, fmt.Errorf("invalid unified diff near line %d: expected hunk header", index+1)
			}
			hunk := patchHunk{
				oldStart: patchNumber(match[1]),
				oldCount: patchCount(match[2]),
				newStart: patchNumber(match[3]),
				newCount: patchCount(match[4]),
			}
			index++
			for index < len(lines) && !strings.HasPrefix(lines[index], "@@ ") && !strings.HasPrefix(lines[index], "--- ") {
				line := lines[index]
				if line == "" && index == len(lines)-1 {
					index++
					break
				}
				if strings.HasPrefix(line, `\ No newline at end of file`) {
					return nil, fmt.Errorf("patches that change a missing final newline are not supported")
				}
				if line == "" || !strings.Contains(" +-", line[:1]) {
					return nil, fmt.Errorf("invalid hunk line %d", index+1)
				}
				hunk.lines = append(hunk.lines, line)
				index++
			}
			file.hunks = append(file.hunks, hunk)
		}
		if len(file.hunks) == 0 {
			return nil, fmt.Errorf("patch for %s has no hunks", file.newPath)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("patch must not be empty")
	}
	return files, nil
}

func patchHeaderPath(header string) string {
	fields := strings.Fields(header)
	if len(fields) == 0 {
		return ""
	}
	path := fields[0]
	if path != "/dev/null" && (strings.HasPrefix(path, "a/") || strings.HasPrefix(path, "b/")) {
		path = path[2:]
	}
	return path
}

func patchNumber(value string) int {
	number, _ := strconv.Atoi(value)
	return number
}

func patchCount(value string) int {
	if value == "" {
		return 1
	}
	return patchNumber(value)
}

func (w *Workspace) prepareUnifiedPatch(patch string) ([]fileChange, error) {
	files, err := parseUnifiedPatch(patch)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	changes := make([]fileChange, 0, len(files))
	for _, file := range files {
		path := file.newPath
		if path == "/dev/null" {
			path = file.oldPath
		}
		resolved, err := w.resolve(path, true)
		if err != nil {
			return nil, err
		}
		if seen[resolved] {
			return nil, fmt.Errorf("duplicate patch target %q", path)
		}
		seen[resolved] = true
		before, readErr := os.ReadFile(resolved)
		existed := readErr == nil
		if readErr != nil && !os.IsNotExist(readErr) {
			return nil, readErr
		}
		if file.oldPath == "/dev/null" && existed {
			return nil, fmt.Errorf("cannot create existing file %q", path)
		}
		if file.oldPath != "/dev/null" && !existed {
			return nil, fmt.Errorf("cannot patch missing file %q", path)
		}
		mode := fs.FileMode(0o644)
		if existed {
			info, err := os.Stat(resolved)
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("target is not a regular file")
			}
			mode = info.Mode().Perm()
		}
		after, err := applyHunks(before, file.hunks)
		if err != nil {
			return nil, fmt.Errorf("apply patch to %s: %w", path, err)
		}
		if file.oldPath == "/dev/null" && len(after) > 0 {
			after = append(after, '\n')
		}
		if file.newPath == "/dev/null" && len(after) != 0 {
			return nil, fmt.Errorf("deletion patch for %s does not remove the entire file", path)
		}
		changes = append(changes, fileChange{
			path:        resolved,
			before:      before,
			after:       after,
			existed:     existed,
			afterExists: file.newPath != "/dev/null",
			mode:        mode,
		})
	}
	return changes, nil
}

func applyHunks(contents []byte, hunks []patchHunk) ([]byte, error) {
	source := splitPatchLines(contents)
	result := make([]string, 0, len(source))
	cursor := 0
	for _, hunk := range hunks {
		start := hunk.oldStart - 1
		if hunk.oldStart == 0 {
			start = 0
		}
		if start < cursor || start > len(source) {
			return nil, fmt.Errorf("hunk starts outside file at old line %d", hunk.oldStart)
		}
		result = append(result, source[cursor:start]...)
		cursor = start
		oldSeen, newSeen := 0, 0
		for _, line := range hunk.lines {
			text := line[1:]
			switch line[0] {
			case ' ':
				if cursor >= len(source) || source[cursor] != text {
					return nil, fmt.Errorf("context mismatch at old line %d", cursor+1)
				}
				result = append(result, text)
				cursor++
				oldSeen++
				newSeen++
			case '-':
				if cursor >= len(source) || source[cursor] != text {
					return nil, fmt.Errorf("deletion mismatch at old line %d", cursor+1)
				}
				cursor++
				oldSeen++
			case '+':
				result = append(result, text)
				newSeen++
			}
		}
		if oldSeen != hunk.oldCount || newSeen != hunk.newCount {
			return nil, fmt.Errorf("hunk count mismatch: expected -%d +%d, got -%d +%d", hunk.oldCount, hunk.newCount, oldSeen, newSeen)
		}
	}
	result = append(result, source[cursor:]...)
	return []byte(strings.Join(result, "\n")), nil
}

func splitPatchLines(contents []byte) []string {
	if len(contents) == 0 {
		return nil
	}
	return strings.Split(string(contents), "\n")
}

func (w *Workspace) prepare(inputs []changeInput) ([]fileChange, error) {
	seen := map[string]bool{}
	changes := make([]fileChange, 0, len(inputs))
	for _, in := range inputs {
		p, e := w.resolve(in.Path, true)
		if e != nil {
			return nil, e
		}
		if seen[p] {
			return nil, fmt.Errorf("duplicate target path %q", in.Path)
		}
		seen[p] = true
		b, e := os.ReadFile(p)
		exists := e == nil
		mode := fs.FileMode(0644)
		if exists {
			info, se := os.Stat(p)
			if se != nil {
				return nil, se
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("target is not a regular file")
			}
			mode = info.Mode().Perm()
		} else if !os.IsNotExist(e) {
			return nil, e
		}
		if in.OldStr == in.NewStr {
			return nil, fmt.Errorf("old_str and new_str must differ")
		}
		if !exists {
			if in.OldStr != "" {
				return nil, fmt.Errorf("cannot replace text in missing file")
			}
		} else {
			if in.OldStr == "" {
				return nil, fmt.Errorf("empty old_str is only for creation")
			}
			n := strings.Count(string(b), in.OldStr)
			if n != 1 {
				return nil, fmt.Errorf("old_str must occur exactly once (found %d)", n)
			}
		}
		after := []byte(in.NewStr)
		if exists {
			after = []byte(strings.Replace(string(b), in.OldStr, in.NewStr, 1))
		}
		changes = append(changes, fileChange{
			path:        p,
			before:      b,
			after:       after,
			existed:     exists,
			afterExists: true,
			mode:        mode,
		})
	}
	return changes, nil
}

func (w *Workspace) applyInputs(inputs []changeInput) (string, error) {
	changes, e := w.prepare(inputs)
	if e != nil {
		return "", e
	}
	return w.applyChanges(changes)
}

func (w *Workspace) applyChanges(changes []fileChange) (string, error) {
	diff := w.diff(changes, false)
	if !w.requestApproval(ApprovalRequest{
		Kind:   ApprovalChanges,
		Title:  "Review file changes",
		Detail: diff,
		Prompt: "Apply changes? [y/N] ",
	}) {
		return "Declined; no files changed.", nil
	}
	if err := w.writeChanges(changes, false); err != nil {
		return "", err
	}
	w.last = cloneChanges(changes)
	return "Applied successfully.\n" + diff, nil
}

func (w *Workspace) requestApproval(request ApprovalRequest) bool {
	// Approval data can include workspace content and command arguments.
	request.Title = sanitizeTerminalText(request.Title)
	request.Detail = sanitizeTerminalText(request.Detail)
	request.Prompt = sanitizeTerminalText(request.Prompt)
	if w.contextErr() != nil {
		return false
	}
	if w.autoApprove {
		return true
	}
	if w.approve != nil {
		return w.approve(w.ctx, request)
	}
	if request.Detail != "" {
		fmt.Fprint(w.output, request.Detail)
	}
	return w.confirmPrompt(request.Prompt)
}

func (w *Workspace) confirmPrompt(prompt string) bool {
	if w.contextErr() != nil {
		return false
	}
	if w.autoApprove {
		return true
	}
	fmt.Fprint(w.output, prompt)
	for {
		answer, err := w.input.ReadString('\n')
		answer = strings.TrimSpace(answer)
		answer = strings.TrimPrefix(answer, "\x1b[200~")
		answer = strings.TrimSuffix(answer, "\x1b[201~")
		answer = strings.ToLower(strings.TrimSpace(answer))
		switch answer {
		case "y", "yes":
			return true
		case "", "n", "no":
			return false
		}
		if err != nil {
			return false
		}
		fmt.Fprint(w.output, "Please enter y or n: ")
	}
}

func (w *Workspace) writeChanges(changes []fileChange, reverse bool) error {
	if err := w.validateChangePreimages(changes, reverse); err != nil {
		return err
	}
	done := []fileChange{}
	for _, c := range changes {
		if err := w.contextErr(); err != nil {
			return combineRollbackError(fmt.Errorf("write cancelled: %w", err), w.rollback(done, reverse))
		}
		data := c.after
		exists := c.afterExists
		mode := c.mode
		if reverse {
			data = c.before
			exists = c.existed
		}
		resolved, err := w.resolve(c.path, true)
		if err != nil || resolved != c.path {
			rollbackErr := w.rollback(done, reverse)
			if err != nil {
				return combineRollbackError(err, rollbackErr)
			}
			return combineRollbackError(fmt.Errorf("target path changed during write"), rollbackErr)
		}
		if exists {
			if e := os.MkdirAll(filepath.Dir(c.path), 0755); e != nil {
				return combineRollbackError(e, w.rollback(done, reverse))
			}
			if e := atomicWriteFile(c.path, data, mode); e != nil {
				return combineRollbackError(e, w.rollback(done, reverse))
			}
		} else if e := os.Remove(c.path); e != nil && !os.IsNotExist(e) {
			return combineRollbackError(e, w.rollback(done, reverse))
		}
		done = append(done, c)
	}
	return nil
}

func (w *Workspace) validateChangePreimages(changes []fileChange, reverse bool) error {
	for _, change := range changes {
		resolved, err := w.resolve(change.path, !change.existed)
		if err != nil && !(reverse && !change.afterExists && os.IsNotExist(err)) {
			return err
		}
		if resolved != "" && resolved != change.path {
			return fmt.Errorf("target path changed since diff was prepared")
		}
		expected, exists := change.before, change.existed
		if reverse {
			expected, exists = change.after, change.afterExists
		}
		contents, err := os.ReadFile(change.path)
		if exists && (err != nil || !bytes.Equal(contents, expected)) {
			return fmt.Errorf("refusing to overwrite %s: file changed since diff was prepared", filepath.Base(change.path))
		}
		if exists {
			info, statErr := os.Stat(change.path)
			if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != change.mode {
				return fmt.Errorf("refusing to overwrite %s: file metadata changed since diff was prepared", filepath.Base(change.path))
			}
		}
		if !exists && !os.IsNotExist(err) {
			return fmt.Errorf("refusing to overwrite %s: path now exists", filepath.Base(change.path))
		}
	}
	return nil
}

func atomicWriteFile(path string, contents []byte, mode fs.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".meldra-write-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(contents); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func (w *Workspace) rollback(done []fileChange, reverse bool) error {
	var rollbackErrors []string
	for i := len(done) - 1; i >= 0; i-- {
		c := done[i]
		data, exists := c.before, c.existed
		if reverse {
			data, exists = c.after, c.afterExists
		}
		if exists {
			if err := atomicWriteFile(c.path, data, c.mode); err != nil {
				rollbackErrors = append(rollbackErrors, err.Error())
			}
		} else {
			if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
				rollbackErrors = append(rollbackErrors, err.Error())
			}
		}
	}
	if len(rollbackErrors) > 0 {
		return fmt.Errorf("%s", strings.Join(rollbackErrors, "; "))
	}
	return nil
}

func combineRollbackError(operationErr, rollbackErr error) error {
	if rollbackErr == nil {
		return operationErr
	}
	return fmt.Errorf("%w; rollback incomplete: %v", operationErr, rollbackErr)
}
func cloneChanges(in []fileChange) []fileChange {
	out := make([]fileChange, len(in))
	copy(out, in)
	for i := range out {
		out[i].before = bytes.Clone(out[i].before)
		out[i].after = bytes.Clone(out[i].after)
	}
	return out
}

func (w *Workspace) undo(raw json.RawMessage) (string, error) {
	var in struct{}
	if e := decodeToolInput(raw, &in); e != nil {
		return "", e
	}
	if len(w.last) == 0 {
		return "", fmt.Errorf("no successful change to undo")
	}
	for _, c := range w.last {
		b, err := os.ReadFile(c.path)
		matches := c.afterExists && err == nil && bytes.Equal(b, c.after)
		if !c.afterExists {
			matches = os.IsNotExist(err)
		}
		if !matches {
			return "", fmt.Errorf("cannot undo: %s changed since it was written", filepath.Base(c.path))
		}
	}
	diff := w.diff(w.last, true)
	if !w.requestApproval(ApprovalRequest{
		Kind:   ApprovalChanges,
		Title:  "Review undo changes",
		Detail: diff,
		Prompt: "Apply changes? [y/N] ",
	}) {
		return "Declined; no files changed.", nil
	}
	if e := w.writeChanges(w.last, true); e != nil {
		return "", e
	}
	w.last = nil
	return "Undo successful.\n" + diff, nil
}

func (w *Workspace) diff(changes []fileChange, reverse bool) string {
	var out strings.Builder
	for _, c := range changes {
		rel, _ := filepath.Rel(w.root, c.path)
		a, b := c.before, c.after
		aExists, bExists := c.existed, c.afterExists
		if reverse {
			a, b = b, a
			aExists, bExists = bExists, aExists
		}
		oldPath, newPath := "a/"+filepath.ToSlash(rel), "b/"+filepath.ToSlash(rel)
		if !aExists {
			oldPath = "/dev/null"
		}
		if !bExists {
			newPath = "/dev/null"
		}
		fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldPath, newPath)
		fmt.Fprintf(&out, "@@ -1,%d +1,%d @@\n", lineCount(a), lineCount(b))
		writeDiffLines(&out, '-', a)
		writeDiffLines(&out, '+', b)
	}
	return out.String()
}

func writeDiffLines(out *strings.Builder, prefix byte, contents []byte) {
	if len(contents) == 0 {
		return
	}
	hasFinalNewline := contents[len(contents)-1] == '\n'
	text := string(contents)
	if hasFinalNewline {
		text = strings.TrimSuffix(text, "\n")
	}
	for _, line := range strings.Split(text, "\n") {
		fmt.Fprintf(out, "%c%s\n", prefix, line)
	}
	if !hasFinalNewline {
		out.WriteString("\\ No newline at end of file\n")
	}
}

func lineCount(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	count := bytes.Count(b, []byte{'\n'})
	if b[len(b)-1] != '\n' {
		count++
	}
	return count
}

func (w *Workspace) runCommand(raw json.RawMessage) (string, error) {
	var in struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Timeout int      `json:"timeout"`
	}
	if e := decodeToolInput(raw, &in, "command"); e != nil {
		return "", e
	}
	return w.execute(in.Command, in.Args, in.Timeout)
}
func allowed(command string, args []string) bool {
	if command != filepath.Base(command) || !allSafeArgs(args) {
		return false
	}
	switch command {
	case "python3":
		return len(args) == 1 && strings.HasSuffix(strings.ToLower(args[0]), ".py")
	case "go":
		return allowedGo(args)
	case "gofmt":
		if len(args) <= 1 || args[0] != "-d" {
			return false
		}
		for _, argument := range args[1:] {
			if strings.HasPrefix(argument, "-") {
				return false
			}
		}
		return true
	case "make":
		return len(args) > 0 && allIn(args, []string{"check", "test"})
	case "git":
		return allowedGit(args)
	}
	return false
}

func allowedGo(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "run":
		if len(args) != 2 || !allSafeArgs(args[1:]) || strings.ContainsAny(args[1], "*?[]") || strings.Contains(args[1], "...") {
			return false
		}
		if args[1] != "." && !strings.HasPrefix(args[1], "."+string(filepath.Separator)) {
			return false
		}
		target := filepath.Clean(args[1])
		return target != ".." && !strings.HasPrefix(target, ".."+string(filepath.Separator))
	case "build":
		hasPackage := false
		for _, argument := range args[1:] {
			if argument == "./..." {
				hasPackage = true
				continue
			}
			if argument != "-trimpath" {
				return false
			}
		}
		return hasPackage
	case "vet":
		return len(args) == 1 || (len(args) == 2 && args[1] == "./...")
	case "test":
		for _, argument := range args[1:] {
			if argument == "." || argument == "./..." || argument == "-race" || argument == "-short" || argument == "-v" || strings.HasPrefix(argument, "-run=") || strings.HasPrefix(argument, "-count=") {
				continue
			}
			return false
		}
		return true
	default:
		return false
	}
}

func allowedGit(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, argument := range args[1:] {
		if !allSafeArgs([]string{argument}) {
			return false
		}
	}
	switch args[0] {
	case "status":
		return allIn(args[1:], []string{"--short", "--porcelain"})
	case "diff":
		return allIn(args[1:], []string{"--", "--cached", "--stat", "--name-only", "--name-status"})
	case "log":
		for _, argument := range args[1:] {
			if argument == "--oneline" || argument == "--stat" || argument == "--" || regexp.MustCompile(`^-[1-9][0-9]*$`).MatchString(argument) {
				continue
			}
			return false
		}
		return true
	default:
		return false
	}
}
func allIn(xs, ok []string) bool {
	for _, x := range xs {
		found := false
		for _, y := range ok {
			if x == y {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func allSafeArgs(args []string) bool {
	for _, a := range args {
		if strings.ContainsAny(a, "\x00\n\r") || filepath.IsAbs(a) || a == ".." || strings.HasPrefix(a, ".."+string(filepath.Separator)) || a == "-C" || strings.HasPrefix(a, "-C=") || strings.HasPrefix(a, "--git-dir") || strings.HasPrefix(a, "--work-tree") || strings.HasPrefix(a, "--output") || a == "--ext-diff" || a == "--textconv" || strings.HasPrefix(a, "--open-files-in-pager") || strings.HasPrefix(a, "-exec") || strings.HasPrefix(a, "-toolexec") || strings.HasPrefix(a, "-vettool") || a == "-o" || strings.HasPrefix(a, "-o=") || strings.HasPrefix(a, "-coverprofile") {
			return false
		}
	}
	return true
}
func (w *Workspace) execute(command string, args []string, seconds int) (string, error) {
	if !allowed(command, args) {
		return "", fmt.Errorf("command is not allowlisted")
	}
	executable, err := w.trustedExecutable(command)
	if err != nil {
		return "", err
	}
	if command == "git" {
		topLevel, err := gitTopLevel(w.root, executable)
		if err != nil {
			return "", err
		}
		if topLevel != w.root {
			return "", fmt.Errorf("workspace must be the Git repository root (%s)", topLevel)
		}
		gitArgs := []string{"-c", "core.fsmonitor=false"}
		if len(args) > 0 && (args[0] == "diff" || args[0] == "show" || args[0] == "log") {
			gitArgs = append(gitArgs, args[0], "--no-ext-diff", "--no-textconv")
			args = append(gitArgs, args[1:]...)
		} else {
			args = append(gitArgs, args...)
		}
	}
	if command == "gofmt" {
		for _, argument := range args[1:] {
			path, err := w.resolve(argument, false)
			if err != nil {
				return "", err
			}
			info, err := os.Stat(path)
			if err != nil {
				return "", err
			}
			if !info.Mode().IsRegular() || filepath.Ext(path) != ".go" {
				return "", fmt.Errorf("gofmt paths must be Go files inside the workspace")
			}
		}
		args = append([]string{"-d", "--"}, args[1:]...)
	}
	if command == "go" && len(args) == 2 && args[0] == "run" {
		path, err := w.resolve(args[1], false)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.IsDir() && (!info.Mode().IsRegular() || filepath.Ext(path) != ".go") {
			return "", fmt.Errorf("go run target must be a workspace directory or Go file")
		}
	}
	if command == "python3" {
		path, err := w.resolve(args[0], false)
		if err != nil {
			return "", err
		}
		info, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || strings.ToLower(filepath.Ext(path)) != ".py" {
			return "", fmt.Errorf("python3 target must be a workspace Python file")
		}
	}
	if seconds == 0 {
		seconds = defaultTimeout
	}
	if seconds < 1 || seconds > 120 {
		return "", fmt.Errorf("timeout must be 1..120 seconds")
	}
	if w.contextErr() != nil {
		return "Command cancelled before start.\n[cancelled]", nil
	}
	if commandRequiresApproval(command) && !w.confirmCommand(command, args) {
		return "Declined; command not run.", nil
	}
	parent := w.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(seconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Dir = w.root
	cmd.Env = safeCommandEnvironment()
	var b limitedBuffer
	b.limit = maxToolOutput
	cmd.Stdout = &b
	cmd.Stderr = &b
	e := runCommandProcess(ctx, cmd)
	status := 0
	if e != nil {
		if ee := new(exec.ExitError); errors.As(e, &ee) {
			status = ee.ExitCode()
		} else if ctx.Err() != nil {
			status = -1
		} else {
			return "", e
		}
	}
	suffix := ""
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		suffix = "\n[timed out]"
	} else if errors.Is(ctx.Err(), context.Canceled) {
		suffix = "\n[cancelled]"
	}
	return fmt.Sprintf("command: %s %s\nstatus: %d\n%s", command, strings.Join(args, " "), status, b.String()) + suffix, nil
}

func (w *Workspace) trustedExecutable(command string) (string, error) {
	path, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("find executable %s: %w", command, err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve executable %s: %w", command, err)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve executable %s: %w", command, err)
	}
	canonical = filepath.Clean(canonical)
	if withinFold(w.root, canonical) {
		return "", fmt.Errorf("refusing to execute %s from inside the workspace", command)
	}
	for _, protected := range w.protected {
		if withinFold(protected, canonical) {
			return "", fmt.Errorf("refusing to execute %s from Meldra configuration storage", command)
		}
	}
	return canonical, nil
}

func commandRequiresApproval(command string) bool {
	return command == "python3" || command == "go" || command == "make"
}

func (w *Workspace) confirmCommand(command string, args []string) bool {
	var rendered strings.Builder
	rendered.WriteString(command)
	for _, argument := range args {
		rendered.WriteByte(' ')
		rendered.WriteString(strconv.Quote(argument))
	}
	text := rendered.String()
	return w.requestApproval(ApprovalRequest{
		Kind:   ApprovalCommand,
		Title:  "Run command",
		Detail: text,
		Prompt: fmt.Sprintf("Run command? %s [y/N] ", text),
	})
}

func safeCommandEnvironment() []string {
	blocked := []string{"OPENAI_API_KEY=", "MELDRA_", "GOFLAGS=", "GIT_DIR=", "GIT_WORK_TREE=", "GIT_EXTERNAL_DIFF=", "GIT_EXEC_PATH=", "GIT_CONFIG=", "GIT_CONFIG_", "GIT_PAGER=", "PAGER=", "PYTHONHOME=", "PYTHONPATH=", "PYTHONSTARTUP=", "PYTHONINSPECT="}
	var environment []string
	for _, variable := range os.Environ() {
		allowed := true
		for _, prefix := range blocked {
			if strings.HasPrefix(variable, prefix) {
				allowed = false
				break
			}
		}
		if allowed {
			environment = append(environment, variable)
		}
	}
	return append(environment, "GOFLAGS=", "GIT_PAGER=cat", "PAGER=cat")
}

type limitedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	room := b.limit - b.Len()
	if room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
			b.truncated = true
		} else {
			b.Buffer.Write(p)
		}
	} else {
		b.truncated = true
	}
	return n, nil
}
func (b *limitedBuffer) String() string {
	s := b.Buffer.String()
	if b.truncated {
		s += "\n[output truncated]"
	}
	return s
}

func (w *Workspace) verify(raw json.RawMessage) (string, error) {
	var in struct {
		Preset string `json:"preset"`
	}
	if e := decodeToolInput(raw, &in, "preset"); e != nil {
		return "", e
	}
	switch in.Preset {
	case "test":
		return w.execute("go", []string{"test", "./..."}, 120)
	case "check":
		if _, err := os.Stat(filepath.Join(w.root, "Makefile")); err == nil {
			return w.execute("make", []string{"check"}, 120)
		}
		vet, err := w.execute("go", []string{"vet", "./..."}, 120)
		if err != nil {
			return "", err
		}
		test, err := w.execute("go", []string{"test", "./..."}, 120)
		return vet + "\n" + test, err
	case "build":
		return w.execute("go", []string{"build", "./..."}, 120)
	case "format":
		args := []string{"-d"}
		err := w.walk(w.root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() && d.Type()&os.ModeSymlink == 0 && strings.HasSuffix(path, ".go") {
				rel, _ := filepath.Rel(w.root, path)
				args = append(args, rel)
			}
			return err
		})
		if err != nil {
			return "", err
		}
		if len(args) == 1 {
			return "command: gofmt -d\nstatus: 0\n(no Go files)", nil
		}
		return w.execute("gofmt", args, 120)
	case "diff":
		return w.execute("git", []string{"diff", "--"}, 60)
	}
	return "", fmt.Errorf("unknown preset")
}
func (w *Workspace) gitReview(raw json.RawMessage) (string, error) {
	var in struct{}
	if e := decodeToolInput(raw, &in); e != nil {
		return "", e
	}
	gitExecutable, err := w.trustedExecutable("git")
	if err != nil {
		return "", err
	}
	topLevel, err := gitTopLevel(w.root, gitExecutable)
	if err != nil {
		return "", err
	}
	if topLevel != w.root {
		return "", fmt.Errorf("workspace must be the Git repository root (%s)", topLevel)
	}
	var out strings.Builder
	for _, x := range [][]string{{"status", "--short"}, {"diff", "--cached", "--"}, {"diff", "--"}, {"log", "-5", "--oneline"}} {
		r, e := w.execute("git", x, 60)
		if e != nil {
			return "", e
		}
		out.WriteString(r)
		out.WriteString("\n")
	}
	return capText(out.String()), nil
}

func gitTopLevel(workspace, gitExecutable string) (string, error) {
	check := exec.Command(gitExecutable, "-c", "core.fsmonitor=false", "rev-parse", "--show-toplevel")
	check.Dir = workspace
	check.Env = safeCommandEnvironment()
	output, err := check.Output()
	if err != nil {
		return "", fmt.Errorf("workspace is not a git repository")
	}
	topLevel, err := filepath.EvalSymlinks(strings.TrimSpace(string(output)))
	if err != nil {
		return "", fmt.Errorf("resolve Git repository root: %w", err)
	}
	return filepath.Clean(topLevel), nil
}
func capText(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return s[:maxToolOutput] + "\n[output truncated]"
}

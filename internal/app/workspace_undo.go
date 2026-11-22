package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

func (b *changeBudget) add(before, after int) error {
	if before < 0 || after < 0 || before > maxEditableFileBytes || after > maxEditableFileBytes {
		return fmt.Errorf("a changed file exceeds the %d byte editable-file limit", maxEditableFileBytes)
	}
	added := int64(before) + int64(after)
	if b.total+added > maxChangeContentBytes {
		return fmt.Errorf("changes exceed the %d byte combined before/after limit", maxChangeContentBytes)
	}
	b.total += added
	return nil
}

func validateChangeCount(count int) error {
	if count == 0 {
		return fmt.Errorf("at least one file change is required")
	}
	if count > maxChangeFiles {
		return fmt.Errorf("changes affect %d files, exceeding the %d file limit", count, maxChangeFiles)
	}
	return nil
}

type fileChange struct {
	path                 string
	before, after        []byte
	existed, afterExists bool
	mode                 fs.FileMode
	createdDirs          []string
}

func validateChangeInput(in changeInput) error {
	if len(in.OldStr) > maxEditableFileBytes {
		return fmt.Errorf("old_str for %q exceeds the %d byte editable-file limit", in.Path, maxEditableFileBytes)
	}
	if len(in.NewStr) > maxEditableFileBytes {
		return fmt.Errorf("new_str for %q exceeds the %d byte editable-file limit", in.Path, maxEditableFileBytes)
	}
	return nil
}

type changeInput struct {
	Path   string `json:"path"`
	OldStr string `json:"old_str"`
	NewStr string `json:"new_str"`
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
		if len(file.hunks) == 0 && !isRenamePatch(file) {
			return nil, fmt.Errorf("patch for %s has no hunks", file.newPath)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("patch must not be empty")
	}
	return files, nil
}

func isRenamePatch(file filePatch) bool {
	return file.oldPath != "/dev/null" && file.newPath != "/dev/null" && file.oldPath != file.newPath
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

func validatePreparedChanges(changes []fileChange) error {
	if err := validateChangeCount(len(changes)); err != nil {
		return err
	}
	var budget changeBudget
	for _, change := range changes {
		if err := budget.add(len(change.before), len(change.after)); err != nil {
			return err
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
	if err := os.Rename(tempPath, path); err != nil {
		return err
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
		out[i].createdDirs = slices.Clone(out[i].createdDirs)
	}
	return out
}

func (w *Workspace) undo(raw json.RawMessage) (string, error) {
	var in struct{}
	if e := decodeToolInput(raw, &in); e != nil {
		return "", e
	}
	if len(w.undoHistory) == 0 {
		return "", fmt.Errorf("no successful change to undo")
	}
	changes := w.undoHistory[len(w.undoHistory)-1]
	if err := w.contextBeforeChanges(changes); err != nil {
		return "", err
	}
	for _, c := range changes {
		b, exists, _, err := readEditableFile(c.path)
		matches := c.afterExists && err == nil && exists && bytes.Equal(b, c.after)
		if !c.afterExists {
			matches = err == nil && !exists
		}
		if !matches {
			return "", fmt.Errorf("cannot undo: %s changed since it was written", filepath.Base(c.path))
		}
	}
	if err := validatePreparedChanges(changes); err != nil {
		return "", err
	}
	diff, err := w.diff(changes, true)
	if err != nil {
		return "", err
	}
	if !w.requestApproval(ApprovalRequest{
		Kind:           ApprovalChanges,
		WorkspaceState: changeFingerprints(changes, true),
		Title:          "Review undo changes",
		Detail:         diff,
		Prompt:         "Apply changes? [y/N] ",
	}) {
		return "Declined; no files changed.", nil
	}
	if err := w.contextBeforeChanges(changes); err != nil {
		return "", err
	}
	if e := w.writeChanges(changes, true); e != nil {
		return "", e
	}
	w.undoHistory = w.undoHistory[:len(w.undoHistory)-1]
	status := "Undo successful."
	if remaining := len(w.undoHistory); remaining > 0 {
		status += fmt.Sprintf(" %d earlier change(s) remain undoable.", remaining)
	}
	return status + "\n" + diff, nil
}

func (w *Workspace) pendingUndo() ([]fileChange, bool) {
	if len(w.undoHistory) == 0 {
		return nil, false
	}
	return w.undoHistory[len(w.undoHistory)-1], true
}

func (w *Workspace) replaceTopUndo(changes []fileChange) {
	if len(w.undoHistory) == 0 || changeBytes(changes) > maxUndoEntryBytes {
		return
	}
	w.undoHistory[len(w.undoHistory)-1] = cloneChanges(changes)
}

func (w *Workspace) pushUndo(changes []fileChange) {
	if size := changeBytes(changes); size > maxUndoEntryBytes {
		return
	}
	if len(w.undoHistory) == maxUndoSteps {
		w.undoHistory = w.undoHistory[1:]
	}
	w.undoHistory = append(w.undoHistory, cloneChanges(changes))
}

func changeBytes(changes []fileChange) int {
	total := 0
	for _, change := range changes {
		total += len(change.before) + len(change.after)
	}
	return total
}

func recordedChangeDirectories(changes []fileChange) []string {
	var directories []string
	for _, change := range changes {
		directories = append(directories, change.createdDirs...)
	}
	return directories
}

const noFinalNewlineMarker = "\\ No newline at end of file\n"

func diffLineSize(contents []byte) int64 {
	if len(contents) == 0 {
		return 0
	}
	size := int64(len(contents)) + int64(lineCount(contents))
	if contents[len(contents)-1] != '\n' {
		size += 1 + int64(len(noFinalNewlineMarker))
	}
	return size
}

func (w *Workspace) diffSize(changes []fileChange, reverse bool) (int64, error) {
	var size int64
	for _, c := range changes {
		rel, err := filepath.Rel(w.root, c.path)
		if err != nil {
			return 0, err
		}
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
		size += int64(len("--- ") + len(oldPath) + 1)
		size += int64(len("+++ ") + len(newPath) + 1)
		size += int64(len("@@ -1,") + len(strconv.Itoa(lineCount(a))) + len(" +1,") + len(strconv.Itoa(lineCount(b))) + len(" @@\n"))
		size += diffLineSize(a) + diffLineSize(b)
	}
	return size, nil
}

func (w *Workspace) diff(changes []fileChange, reverse bool) (string, error) {
	size, err := w.diffSize(changes, reverse)
	if err != nil {
		return "", err
	}
	if size > maxApprovalPreviewBytes {
		return "", fmt.Errorf("approval preview is %d bytes, exceeding the %d byte limit; refusing to apply changes because the full diff cannot be shown", size, maxApprovalPreviewBytes)
	}
	var out strings.Builder
	out.Grow(int(size))
	for _, c := range changes {
		rel, err := filepath.Rel(w.root, c.path)
		if err != nil {
			return "", err
		}
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
	return out.String(), nil
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
	for line := range strings.SplitSeq(text, "\n") {
		fmt.Fprintf(out, "%c%s\n", prefix, line)
	}
	if !hasFinalNewline {
		out.WriteString(noFinalNewlineMarker)
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

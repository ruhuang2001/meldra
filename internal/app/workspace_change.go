package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"meldra/internal/tool"
	"os"
	"path/filepath"
	"strings"
)

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
	if len(in.Patch) > maxPatchBytes {
		return "", fmt.Errorf("patch exceeds the %d byte limit", maxPatchBytes)
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
	if err := validateChangeCount(len(in.Changes)); err != nil {
		return "", err
	}
	return w.applyInputs(in.Changes)
}

func (w *Workspace) prepareUnifiedPatch(patch string) ([]fileChange, error) {
	if len(patch) > maxPatchBytes {
		return nil, fmt.Errorf("patch exceeds the %d byte limit", maxPatchBytes)
	}
	files, err := parseUnifiedPatch(patch)
	if err != nil {
		return nil, err
	}
	if err := validateChangeCount(len(files)); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	changes := make([]fileChange, 0, len(files))
	var budget changeBudget
	for _, file := range files {
		if isRenamePatch(file) {
			renamed, err := w.preparePatchRename(file, seen, &budget)
			if err != nil {
				return nil, err
			}
			if err := validateChangeCount(len(changes) + len(renamed)); err != nil {
				return nil, err
			}
			changes = append(changes, renamed...)
			continue
		}
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
		before, existed, mode, readErr := readEditableFile(resolved)
		if readErr != nil {
			return nil, readErr
		}
		if file.oldPath == "/dev/null" && existed {
			return nil, fmt.Errorf("cannot create existing file %q", path)
		}
		if file.oldPath != "/dev/null" && !existed {
			return nil, fmt.Errorf("cannot patch missing file %q", path)
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
		if err := budget.add(len(before), len(after)); err != nil {
			return nil, err
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

func (w *Workspace) preparePatchRename(file filePatch, seen map[string]bool, budget *changeBudget) ([]fileChange, error) {
	oldResolved, err := w.resolve(file.oldPath, true)
	if err != nil {
		return nil, err
	}
	newResolved, err := w.resolve(file.newPath, true)
	if err != nil {
		return nil, err
	}
	if seen[oldResolved] || seen[newResolved] {
		return nil, fmt.Errorf("duplicate patch target %q", file.newPath)
	}
	seen[oldResolved], seen[newResolved] = true, true
	before, existed, mode, err := readEditableFile(oldResolved)
	if err != nil {
		return nil, err
	}
	if !existed {
		return nil, fmt.Errorf("cannot rename missing file %q", file.oldPath)
	}
	after, err := applyHunks(before, file.hunks)
	if err != nil {
		return nil, fmt.Errorf("apply patch to %s: %w", file.oldPath, err)
	}
	existing, newExists, _, err := readEditableFile(newResolved)
	if err != nil {
		return nil, err
	}
	if newExists {
		return nil, fmt.Errorf("cannot rename onto existing file %q", file.newPath)
	}
	if err := budget.add(len(before), len(after)); err != nil {
		return nil, err
	}
	return []fileChange{
		{path: oldResolved, before: before, after: before, existed: true, afterExists: false, mode: mode},
		{path: newResolved, before: existing, after: after, existed: false, afterExists: true, mode: mode},
	}, nil
}

func (w *Workspace) prepare(inputs []changeInput) ([]fileChange, error) {
	if err := validateChangeCount(len(inputs)); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	changes := make([]fileChange, 0, len(inputs))
	var budget changeBudget
	for _, in := range inputs {
		if err := validateChangeInput(in); err != nil {
			return nil, err
		}
		p, e := w.resolve(in.Path, true)
		if e != nil {
			return nil, e
		}
		if seen[p] {
			return nil, fmt.Errorf("duplicate target path %q", in.Path)
		}
		seen[p] = true
		b, exists, mode, e := readEditableFile(p)
		if e != nil {
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
			contents := string(b)
			n := strings.Count(contents, in.OldStr)
			if n != 1 {
				return nil, fmt.Errorf("old_str must occur exactly once (found %d)", n)
			}
			if len(b)-len(in.OldStr)+len(in.NewStr) > maxEditableFileBytes {
				return nil, fmt.Errorf("replacement for %q exceeds the %d byte editable-file limit", in.Path, maxEditableFileBytes)
			}
			after := []byte(strings.Replace(contents, in.OldStr, in.NewStr, 1))
			if err := budget.add(len(b), len(after)); err != nil {
				return nil, err
			}
			changes = append(changes, fileChange{
				path:        p,
				before:      b,
				after:       after,
				existed:     true,
				afterExists: true,
				mode:        mode,
			})
			continue
		}
		after := []byte(in.NewStr)
		if err := budget.add(0, len(after)); err != nil {
			return nil, err
		}
		changes = append(changes, fileChange{
			path:        p,
			before:      nil,
			after:       after,
			existed:     false,
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
	if err := w.contextBeforeChanges(changes); err != nil {
		return "", err
	}
	if err := validatePreparedChanges(changes); err != nil {
		return "", err
	}
	diff, err := w.diff(changes, false)
	if err != nil {
		return "", err
	}
	if !w.requestApproval(ApprovalRequest{
		Kind:           ApprovalChanges,
		WorkspaceState: changeFingerprints(changes, false),
		Title:          "Review file changes",
		Detail:         diff,
		Prompt:         "Apply changes? [y/N] ",
	}) {
		return "Declined; no files changed.", nil
	}
	if err := w.contextBeforeChanges(changes); err != nil {
		return "", err
	}
	if err := w.writeChanges(changes, false); err != nil {
		return "", err
	}
	w.pushUndo(changes)
	return "Applied successfully.\n" + diff, nil
}

func (w *Workspace) requestApproval(request ApprovalRequest) (approved bool) {
	request.Source = "human"
	_, permissionProfile, _ := w.policy.snapshot()
	if w.autoApprove {
		request.Source = "auto-approve"
	} else if permissionProfile == PermissionWorkspaceEdit && request.Kind == ApprovalChanges {
		request.Source = "workspace-edit-policy"
	}
	checkProjectContext := func() error {
		if request.Kind == ApprovalChanges || w.projectContext == nil {
			return nil
		}
		return w.projectContext.BeforeWrite(nil)
	}
	if err := checkProjectContext(); err != nil {
		tool.Observe(w.ctx, func(o *tool.Observation) { o.Err = err })
		return false
	}
	if c := controlFromContext(w.ctx); c != nil {
		ctx, done := c.approvalContext(w.ctx)
		defer done()
		previous := w.ctx
		w.ctx = ctx
		defer func() { w.ctx = previous }()
	}
	if w.approvalPending == nil && request.Source == "human" && !(w.approvalGrant != nil && w.approvalGrant()) && w.hooks != nil && w.contextErr() == nil {
		if blocked, reason := w.checkPermissionHook(w.ctx, request); blocked {
			tool.Observe(w.ctx, func(o *tool.Observation) {
				o.Result.Status = tool.Declined
				o.Err = fmt.Errorf("declined by PermissionRequest hook: %s", reason)
			})
			return false
		}
	}
	if w.approvalPending != nil && w.contextErr() == nil {
		if err := w.approvalPending(w.ctx, request); err != nil {
			tool.Observe(w.ctx, func(o *tool.Observation) {
				o.Err = err
				if _, blocked := errors.AsType[*hookBlockedError](err); blocked {
					o.Result.Status = tool.Declined
				}
			})
			return false
		}
	}

	defer func() {
		if w.approvalRecord != nil {
			if err := w.approvalRecord(w.ctx, request, approved); err != nil {
				approved = false
				tool.Observe(w.ctx, func(o *tool.Observation) { o.Err = err })
			}
		}
	}()
	defer func() {
		if !approved {
			tool.Observe(w.ctx, func(o *tool.Observation) {
				if o.Result.Status == tool.Unknown {
					return
				}
				o.Result.Status = tool.Declined
				if w.contextErr() != nil {
					o.Result.Status = tool.Cancelled
				}
			})
		}
	}()
	defer func() {
		if approved {
			if err := w.admitOperation(w.ctx); err != nil {
				approved = false
				if w.ctx.Err() == nil {
					tool.Observe(w.ctx, func(o *tool.Observation) { o.Err = err })
				}
			}
			if approved {
				if err := checkProjectContext(); err != nil {
					approved = false
					tool.Observe(w.ctx, func(o *tool.Observation) { o.Err = err })
				}
			}
		}
	}()
	request.Title = sanitizeTerminalText(request.Title)
	request.Detail = sanitizeTerminalText(request.Detail)
	request.Prompt = sanitizeTerminalText(request.Prompt)
	if w.contextErr() != nil {
		return false
	}
	_, permissions, _ := w.policy.snapshot()
	if w.approvalGrant != nil && w.approvalGrant() {
		w.presentApproval(request)
		return true
	}
	if w.autoApprove || permissions == PermissionWorkspaceEdit && request.Kind == ApprovalChanges {
		w.presentApproval(request)
		return true
	}
	if w.approve != nil {
		return w.approve(w.ctx, request)
	}
	w.presentApproval(request)
	return w.confirmPrompt(request.Prompt)
}

func (w *Workspace) presentApproval(request ApprovalRequest) {
	if w.present != nil {
		w.present(request)
		return
	}
	if request.Kind == ApprovalChanges && request.Detail != "" {
		_, _ = fmt.Fprint(w.output, request.Detail)
	}
}

func (w *Workspace) confirmPrompt(prompt string) bool {
	if w.contextErr() != nil {
		return false
	}
	if w.autoApprove {
		return true
	}
	for {
		var answer string
		var err error
		if w.mcpInput != nil {
			var ok bool
			answer, ok = w.mcpInput(w.ctx, prompt)
			if !ok {
				return false
			}
		} else {
			fmt.Fprint(w.output, prompt)
			answer, err = w.input.ReadString('\n')
		}
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
		prompt = "Please enter y or n: "
	}
}

func (w *Workspace) writeChanges(changes []fileChange, reverse bool) error {
	if err := w.admitOperation(w.ctx); err != nil {
		return err
	}
	if err := w.processWriteConflict("apply_patch", nil); err != nil {
		return err
	}
	if err := w.validateChangePreimages(changes, reverse); err != nil {
		return err
	}
	done := []fileChange{}
	createdDirs := []string{}
	for index := range changes {
		c := &changes[index]
		if err := w.contextErr(); err != nil {
			return combineRollbackError(fmt.Errorf("write cancelled: %w", err), w.rollback(done, reverse, createdDirs))
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
			rollbackErr := w.rollback(done, reverse, createdDirs)
			if err != nil {
				return combineRollbackError(err, rollbackErr)
			}
			return combineRollbackError(fmt.Errorf("target path changed during write"), rollbackErr)
		}
		tool.Observe(w.ctx, func(o *tool.Observation) { o.Started = true })
		if exists {
			if !reverse {
				created, e := makeDirectoryTreeDurableTracked(filepath.Dir(c.path), 0o755, w.syncDirectory)
				createdDirs = append(createdDirs, created...)
				c.createdDirs = append(c.createdDirs, created...)
				if e != nil {
					return combineRollbackError(e, w.rollback(done, reverse, createdDirs))
				}
			}
			if e := atomicWriteFile(c.path, data, mode); e != nil {
				return combineRollbackError(e, w.rollback(done, reverse, createdDirs))
			}
		} else {
			if e := os.Remove(c.path); e != nil && !os.IsNotExist(e) {
				return combineRollbackError(e, w.rollback(done, reverse, createdDirs))
			}
		}
		done = append(done, *c)
		if e := w.syncDirectory(filepath.Dir(c.path)); e != nil {
			return combineRollbackError(e, w.rollback(done, reverse, createdDirs))
		}
	}
	if reverse {
		removed, err := w.removeCreatedDirectories(recordedChangeDirectories(changes))
		if err != nil {
			restoreErr := w.restoreCreatedDirectories(removed)
			rollbackErr := w.rollback(done, reverse, createdDirs)
			return combineRollbackError(err, errors.Join(restoreErr, rollbackErr))
		}
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
		contents, currentExists, mode, readErr := readEditableFile(change.path)
		if exists && (readErr != nil || !currentExists || !bytes.Equal(contents, expected)) {
			return fmt.Errorf("refusing to overwrite %s: file changed since diff was prepared", filepath.Base(change.path))
		}
		if exists {
			if mode != change.mode {
				return fmt.Errorf("refusing to overwrite %s: file metadata changed since diff was prepared", filepath.Base(change.path))
			}
		}
		if !exists && readErr != nil {
			return fmt.Errorf("refusing to overwrite %s: %w", filepath.Base(change.path), readErr)
		}
		if !exists && currentExists {
			return fmt.Errorf("refusing to overwrite %s: path now exists", filepath.Base(change.path))
		}
	}
	return nil
}

func (w *Workspace) rollback(done []fileChange, reverse bool, createdDirs []string) error {
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
				continue
			}
			if err := w.syncDirectory(filepath.Dir(c.path)); err != nil {
				rollbackErrors = append(rollbackErrors, err.Error())
			}
		} else {
			if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
				rollbackErrors = append(rollbackErrors, err.Error())
				continue
			}
			if err := w.syncDirectory(filepath.Dir(c.path)); err != nil {
				rollbackErrors = append(rollbackErrors, err.Error())
			}
		}
	}
	if _, err := w.removeCreatedDirectories(createdDirs); err != nil {
		rollbackErrors = append(rollbackErrors, err.Error())
	}
	if len(rollbackErrors) > 0 {
		return fmt.Errorf("%s", strings.Join(rollbackErrors, "; "))
	}
	return nil
}

func (w *Workspace) removeCreatedDirectories(directories []string) ([]string, error) {
	seen := make(map[string]struct{}, len(directories))
	removed := make([]string, 0, len(directories))
	var removalErrors []string
	for index := len(directories) - 1; index >= 0; index-- {
		directory := directories[index]
		if _, ok := seen[directory]; ok {
			continue
		}
		seen[directory] = struct{}{}
		if err := os.Remove(directory); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			removalErrors = append(removalErrors, fmt.Sprintf("remove directory %s: %v", directory, err))
			continue
		}
		removed = append(removed, directory)
		if err := w.syncDirectory(filepath.Dir(directory)); err != nil {
			removalErrors = append(removalErrors, err.Error())
		}
	}
	if len(removalErrors) > 0 {
		return removed, fmt.Errorf("%s", strings.Join(removalErrors, "; "))
	}
	return removed, nil
}

func (w *Workspace) restoreCreatedDirectories(directories []string) error {
	var restoreErrors []string
	for index := len(directories) - 1; index >= 0; index-- {
		directory := directories[index]
		if err := os.Mkdir(directory, 0o755); err != nil && !os.IsExist(err) {
			restoreErrors = append(restoreErrors, fmt.Sprintf("restore directory %s: %v", directory, err))
			continue
		}
		if err := w.syncDirectory(filepath.Dir(directory)); err != nil {
			restoreErrors = append(restoreErrors, err.Error())
		}
	}
	if len(restoreErrors) > 0 {
		return fmt.Errorf("%s", strings.Join(restoreErrors, "; "))
	}
	return nil
}

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"

	"meldra/internal/task"
)

// Undo evidence uses the same file/count/content limits as a prepared edit.
// Contents live in a private, digest-checked artifact, never in the approval UI.
type undoChange struct {
	Path        string      `json:"path"`
	Before      []byte      `json:"before"`
	After       []byte      `json:"after"`
	Existed     bool        `json:"existed"`
	AfterExists bool        `json:"after_exists"`
	Mode        fs.FileMode `json:"mode"`
	CreatedDirs []string    `json:"created_dirs,omitempty"`
}

func (e *taskExecution) saveUndoApproval(ctx context.Context) (task.ArtifactRef, error) {
	changes := e.workspace.last
	if len(changes) == 0 {
		return task.ArtifactRef{}, fmt.Errorf("undo approval has no prepared changes")
	}
	if err := validatePreparedChanges(changes); err != nil {
		return task.ArtifactRef{}, err
	}
	evidence := make([]undoChange, len(changes))
	for i, c := range changes {
		evidence[i] = undoChange{c.path, c.before, c.after, c.existed, c.afterExists, c.mode, c.createdDirs}
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return task.ArtifactRef{}, err
	}
	return e.db.PutArtifact(ctx, e.lease, e.session.ID, "undo-approval.json", raw)
}

func (e *taskExecution) restoreUndoApproval(ctx context.Context, approval task.Approval, detail approvalDetail) error {
	if detail.UndoEvidence == nil {
		return fmt.Errorf("approval has no undo evidence; request a new approval")
	}
	raw, err := e.db.ReadArtifact(ctx, *detail.UndoEvidence)
	if err != nil {
		return fmt.Errorf("read undo approval evidence: %w", err)
	}
	var evidence []undoChange
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return fmt.Errorf("decode undo evidence: %w", err)
	}
	if len(evidence) == 0 {
		return fmt.Errorf("undo evidence is empty")
	}
	if err := validateChangeCount(len(evidence)); err != nil {
		return err
	}
	changes := make([]fileChange, len(evidence))
	for i, c := range evidence {
		path, err := e.workspace.resolve(c.Path, true)
		if err != nil || path != c.Path {
			return fmt.Errorf("undo evidence path is no longer valid")
		}
		for _, dir := range c.CreatedDirs {
			resolved, err := e.workspace.resolve(dir, true)
			if err != nil || resolved != dir || dir == e.workspace.root || !within(dir, filepath.Dir(path)) {
				return fmt.Errorf("undo directory evidence is invalid")
			}
		}
		changes[i] = fileChange{path: path, before: c.Before, after: c.After, existed: c.Existed, afterExists: c.AfterExists, mode: c.Mode, createdDirs: c.CreatedDirs}
	}
	if err := validatePreparedChanges(changes); err != nil {
		return err
	}
	if !bytes.Equal(changeFingerprints(changes, true), approval.WorkspaceState) {
		return fmt.Errorf("undo evidence does not match the approved files")
	}
	state, err := e.workspace.reconcileFiles(approval.WorkspaceState)
	if err != nil {
		return err
	}
	if state != "before" {
		return fmt.Errorf("undo files changed since approval; request a new approval")
	}
	if err := e.workspace.contextBeforeChanges(changes); err != nil {
		return err
	}
	e.workspace.last = cloneChanges(changes)
	return nil
}

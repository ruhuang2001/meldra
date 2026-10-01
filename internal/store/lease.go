package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"meldra/internal/task"
)

// Lease owns one task and canonical workspace until Close. Lock files are not
// deleted: unlinking an advisory lock lets another process lock a new inode.
// A killed process loses its locks automatically, but its records remain.
type Lease struct {
	mu                sync.Mutex
	store             *Store
	taskID            string
	workspace         string
	workspaceIdentity string
	files             []*os.File
	closed            bool
}

func (s *Store) Acquire(ctx context.Context, taskID, workspace string) (*Lease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(taskID) {
		return nil, errors.New("invalid task ID")
	}
	canonical, err := CanonicalWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	workspaceIdentity, err := directoryIdentity(canonical)
	if err != nil {
		return nil, err
	}
	if identity, err := directoryIdentity(s.dir); err != nil || identity != s.dirIdentity {
		return nil, fmt.Errorf("%w: task storage directory changed", task.ErrLease)
	}
	if err := secureDir(s.lockDir); err != nil {
		return nil, err
	}
	l := &Lease{store: s, taskID: taskID, workspace: canonical, workspaceIdentity: workspaceIdentity}
	for _, key := range []string{"task:" + s.dirIdentity + ":" + taskID, "workspace:" + workspaceIdentity} {
		path := filepath.Join(s.lockDir, Hash([]byte(key))+".lock")
		f, err := lockFile(path)
		if err != nil {
			_ = l.Close()
			return nil, fmt.Errorf("acquire execution ownership: %w", err)
		}
		l.files = append(l.files, f)
	}
	if err := ctx.Err(); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}

func (l *Lease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	var errs []error
	for _, f := range l.files {
		errs = append(errs, unlockFile(f))
	}
	l.files = nil
	return errors.Join(errs...)
}

func (s *Store) owned(ctx context.Context, l *Lease, taskID string, fn func(*sql.Tx) error) error {
	if l == nil {
		return task.ErrLease
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.store != s || l.taskID != taskID {
		return task.ErrLease
	}
	if identity, err := directoryIdentity(s.dir); err != nil || identity != s.dirIdentity {
		return fmt.Errorf("%w: task storage directory changed", task.ErrLease)
	}
	if identity, err := directoryIdentity(l.workspace); err != nil || identity != l.workspaceIdentity {
		return fmt.Errorf("%w: workspace directory changed", task.ErrLease)
	}
	return s.transact(ctx, func(tx *sql.Tx) error {
		record, err := getRecord[task.Task](ctx, tx, "SELECT record FROM tasks WHERE id=?", taskID)
		if err != nil {
			return err
		}
		// Paths differing only in case can refer to the same directory on APFS.
		// Compare filesystem identity, and reject a renamed/replaced directory
		// instead of keeping a stale path-based ownership claim.
		identity, identityErr := directoryIdentity(record.Workspace)
		if identityErr != nil || identity != l.workspaceIdentity {
			return fmt.Errorf("%w: workspace changed", task.ErrLease)
		}
		return fn(tx)
	})
}

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"meldra/internal/task"
)

func TestWorkspaceOwnershipUsesFilesystemIdentity(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	parent := t.TempDir()
	workspace, alias := filepath.Join(parent, "CaseWorkspace"), filepath.Join(parent, "caseworkspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(workspace)
	if err != nil {
		t.Fatal(err)
	}
	aliasInfo, err := os.Stat(alias)
	if errors.Is(err, os.ErrNotExist) {
		// Case-sensitive filesystems must allow genuinely different directories;
		// lowercasing paths would incorrectly serialize these two workspaces.
		if err := os.Mkdir(alias, 0700); err != nil {
			t.Fatal(err)
		}
		aliasInfo, err = os.Stat(alias)
	}
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.Acquire(t.Context(), "first", workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	second, err := s.Acquire(t.Context(), "second", alias)
	if os.SameFile(info, aliasInfo) {
		if second != nil {
			_ = second.Close()
		}
		if !errors.Is(err, task.ErrBusy) {
			t.Fatalf("case alias bypassed same-directory lock: %v", err)
		}
		if _, err := s.EnsureTask(t.Context(), task.Task{ID: "first", Goal: "resume same filesystem directory", Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
		if err := l.Close(); err != nil {
			t.Fatal(err)
		}
		resumed, err := s.Acquire(t.Context(), "first", alias)
		if err != nil {
			t.Fatal(err)
		}
		defer resumed.Close()
		if _, err := s.StartRun(t.Context(), resumed, task.Run{TaskID: "first"}); err != nil {
			t.Fatalf("same-inode case alias refused during resume: %v", err)
		}
	} else {
		if err != nil {
			t.Fatalf("different case-sensitive directories conflated: %v", err)
		}
		_ = second.Close()
	}
}

func TestTaskOwnershipUsesStoreFilesystemIdentity(t *testing.T) {
	parent := t.TempDir()
	dir, alias := filepath.Join(parent, "CaseStore"), filepath.Join(parent, "casestore")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	aliasInfo, err := os.Stat(alias)
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.Acquire(t.Context(), "same-task", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	second, err := other.Acquire(t.Context(), "same-task", t.TempDir())
	if os.SameFile(info, aliasInfo) {
		if second != nil {
			_ = second.Close()
		}
		if !errors.Is(err, task.ErrBusy) {
			t.Fatalf("store case alias bypassed task lock: %v", err)
		}
	} else {
		if err != nil {
			t.Fatalf("independent stores conflated: %v", err)
		}
		_ = second.Close()
	}
}

func TestLeaseRejectsReplacedWorkspaceDirectory(t *testing.T) {
	f := setup(t)
	if err := os.Rename(f.workspace, f.workspace+"-original"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(f.workspace+"-original", f.workspace)
	if err := os.Mkdir(f.workspace, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(f.workspace)
	if err := f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, Kind: "must-not-commit"}); !errors.Is(err, task.ErrLease) {
		t.Fatalf("replaced workspace retained old ownership: %v", err)
	}
	events, err := f.s.Events(t.Context(), f.task.ID, 0, 100)
	if err != nil || len(events) != 2 {
		t.Fatalf("replacement mutation persisted: %+v %v", events, err)
	}
}

func TestStoreRejectsReplacedDirectory(t *testing.T) {
	f := setup(t)
	dir := f.s.dir
	if err := os.Rename(dir, dir+"-original"); err != nil {
		t.Fatal(err)
	}
	defer os.Rename(dir+"-original", dir)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, Kind: "must-not-commit"}); !errors.Is(err, task.ErrLease) {
		t.Fatalf("replaced store retained old ownership: %v", err)
	}
	if _, err := f.s.EnsureTask(t.Context(), task.Task{ID: "new-task", Goal: "do not modify detached database", Workspace: t.TempDir()}); !errors.Is(err, task.ErrLease) {
		t.Fatalf("unleased mutation used replaced store: %v", err)
	}
	if _, err := f.s.Acquire(t.Context(), "new-task", t.TempDir()); !errors.Is(err, task.ErrLease) {
		t.Fatalf("acquired lease from replaced store: %v", err)
	}
}

func TestTaskCanResumeThroughSameDirectoryAlias(t *testing.T) {
	f := setup(t)
	if err := f.s.FinishRun(t.Context(), f.l, f.run.ID, task.RunCancelled, "closed"); err != nil {
		t.Fatal(err)
	}
	if err := f.l.Close(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "workspace-alias")
	if err := os.Symlink(f.workspace, alias); err != nil {
		t.Fatal(err)
	}
	l, err := f.s.Acquire(t.Context(), f.task.ID, alias)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err := f.s.StartRun(t.Context(), l, task.Run{TaskID: f.task.ID}); err != nil {
		t.Fatalf("same directory alias rejected: %v", err)
	}
}

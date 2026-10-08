package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/task"
)

func TestLargeWALWriterHelper(t *testing.T) {
	path := os.Getenv("MELDRA_TEST_LARGE_WAL")
	if path == "" {
		return
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; PRAGMA synchronous=OFF; CREATE TABLE wal_fixture(id INTEGER PRIMARY KEY, payload BLOB); INSERT INTO wal_fixture VALUES(1,zeroblob(1048576))"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		if _, err := db.Exec("UPDATE wal_fixture SET payload=randomblob(1048576) WHERE id=1"); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > MaxDatabaseBytes {
			os.Exit(0)
		} // Leave the committed WAL without closing/checkpointing.
	}
	t.Fatal("fixture did not create an oversized WAL")
}

func TestFailedArtifactTransactionCleansOnlyUnreferencedContent(t *testing.T) {
	f := setup(t)
	retained, err := f.s.PutArtifact(t.Context(), f.l, f.task.ID, "retained", []byte("keep"))
	if err != nil {
		t.Fatal(err)
	}
	fault := errors.New("failed artifact commit")
	f.s.beforeCommit = func() error { return fault }
	if _, err := f.s.PutArtifact(t.Context(), f.l, f.task.ID, "same", []byte("keep")); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	if data, err := f.s.ReadArtifact(t.Context(), retained); err != nil || string(data) != "keep" {
		t.Fatalf("committed artifact removed: %q %v", data, err)
	}
	if _, err := f.s.PutArtifact(t.Context(), f.l, f.task.ID, "new", []byte("uncommitted")); !errors.Is(err, fault) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.s.dir, "artifacts", Hash([]byte("uncommitted")))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback orphan retained: %v", err)
	}
	f.s.beforeCommit = nil
	// Crash leftovers without metadata are reclaimed when capacity is needed.
	dir := filepath.Join(f.s.dir, "artifacts")
	orphan := filepath.Join(dir, strings.Repeat("a", 64))
	file, err := os.Create(orphan)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(MaxArtifactTotalBytes)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PutArtifact(t.Context(), f.l, f.task.ID, "next", []byte("next")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("crash orphan not reclaimed")
	}
	if _, err := f.s.ReadArtifact(context.Background(), retained); err != nil {
		t.Fatal("committed save event ignored", err)
	}
}

func TestOpenRecoversWALLargerThanDatabaseLimit(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.dir, "tasks.db")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLargeWALWriterHelper$")
	command.Env = append(os.Environ(), "MELDRA_TEST_LARGE_WAL="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("writer: %s %v", output, err)
	}
	info, err := os.Stat(path + "-wal")
	if err != nil || info.Size() <= MaxDatabaseBytes {
		t.Fatalf("WAL fixture: %v %v", info, err)
	}
	recovered, err := Open(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	var size int
	if err := recovered.db.QueryRow("SELECT length(payload) FROM wal_fixture WHERE id=1").Scan(&size); err != nil || size != 1048576 {
		t.Fatalf("WAL content not recovered: %d %v", size, err)
	}
	if _, err := recovered.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
}

func TestCacheEvictionDoesNotReleaseWorkspaceOwnership(t *testing.T) {
	f := setup(t)
	dirs, err := OwnershipDirectories()
	if err != nil {
		t.Fatal(err)
	}
	legacy := dirs[1]
	moved := legacy + "-evicted"
	if err := os.Rename(legacy, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(legacy); _ = os.Rename(moved, legacy) })
	other, err := Open(filepath.Join(t.TempDir(), "other"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second, err := other.Acquire(t.Context(), "other-task", f.workspace)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, task.ErrBusy) {
		t.Fatalf("cache eviction bypassed persistent ownership: %v", err)
	}
	if err := f.s.AppendEvent(t.Context(), f.l, task.Event{TaskID: f.task.ID, Kind: "must-not-write"}); !errors.Is(err, task.ErrLease) {
		t.Fatalf("replaced lock was not detected: %v", err)
	}
}

func TestLegacyLocksStillExcludeNewBuilds(t *testing.T) {
	f := setup(t)
	if err := f.l.Close(); err != nil {
		t.Fatal(err)
	}
	dirs, err := OwnershipDirectories()
	if err != nil {
		t.Fatal(err)
	}
	// Emulate the older implementation, which only holds the cache namespace.
	f.s.lockDir = dirs[1]
	old, err := f.s.Acquire(t.Context(), f.task.ID, f.workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	newer, err := Open(f.s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Close()
	lease, err := newer.Acquire(t.Context(), f.task.ID, f.workspace)
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(err, task.ErrBusy) {
		t.Fatalf("old build was not excluded: %v", err)
	}
}

func TestInspectionDoesNotRequireOwnershipEnvironment(t *testing.T) {
	f := setup(t)
	if err := f.l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	db, err := Open(f.s.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.GetTask(t.Context(), f.task.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryQueriesOnlyDecodeRelevantHistory(t *testing.T) {
	f := setup(t)
	for i := range 110 {
		call := plan(t, f, fmt.Sprint(i), task.Read)
		if err := f.s.StartTool(t.Context(), f.l, call.ID); err != nil {
			t.Fatal(err)
		}
		status := task.ToolSucceeded
		if i > 4 {
			status = task.ToolUnknown
		}
		if err := f.s.FinishTool(t.Context(), f.l, call.ID, task.Result{Status: status}); err != nil {
			t.Fatal(err)
		}
	}
	// Corrupt an irrelevant old terminal record to prove recovery doesn't decode it.
	if _, err := f.s.db.Exec("UPDATE tool_calls SET record=? WHERE id=(SELECT id FROM tool_calls WHERE status='succeeded' ORDER BY planned LIMIT 1)", `{"unused":true}`); err != nil {
		t.Fatal(err)
	}
	latest, err := f.s.LatestToolCalls(t.Context(), f.task.ID)
	if err != nil || len(latest) != 30 || latest[0].Name != "80" || latest[29].Name != "109" {
		t.Fatalf("latest=%+v %v", latest, err)
	}
	count, after := 0, ""
	for {
		calls, err := f.s.UnknownToolCalls(t.Context(), f.task.ID, after)
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) == 0 {
			break
		}
		if len(calls) > 100 {
			t.Fatal("unbounded page")
		}
		count += len(calls)
		after = calls[len(calls)-1].ID
	}
	if count != 105 {
		t.Fatalf("unknown count=%d", count)
	}
}

func TestArtifactQuotaCleanupAndReferenceValidation(t *testing.T) {
	f := setup(t)
	dir := filepath.Join(f.s.dir, "artifacts")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(dir, ".pending-dead-writer")
	file, err := os.Create(pending)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Truncate(MaxArtifactTotalBytes)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.s.PutArtifact(t.Context(), f.l, f.task.ID, "log", []byte("saved"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale temporary artifact retained")
	}
	call := plan(t, f, "read_file", task.Read)
	if err := f.s.StartTool(t.Context(), f.l, call.ID); err != nil {
		t.Fatal(err)
	}
	ref.SHA256 = strings.Repeat("0", 64)
	if err := f.s.FinishTool(t.Context(), f.l, call.ID, task.Result{Status: task.ToolSucceeded, Artifacts: []task.ArtifactRef{ref}}); err == nil {
		t.Fatal("mismatched digest accepted")
	}
}

func TestPrivateRootAliasesDoNotPermitArtifactSymlinks(t *testing.T) {
	parent := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(parent, alias); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(alias, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.Symlink(t.TempDir(), filepath.Join(s.dir, "artifacts")); err != nil {
		t.Fatal(err)
	}
	record, err := s.EnsureTask(t.Context(), task.Task{ID: "t", Goal: "test", Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.Acquire(t.Context(), record.ID, record.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := s.PutArtifact(t.Context(), lease, record.ID, "bad", []byte("bad")); err == nil {
		t.Fatal("private artifact symlink accepted")
	}
}

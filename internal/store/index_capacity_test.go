package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndexMigrationAtCapacityPreservesReopenability(t *testing.T) {
	f := setup(t)
	if err := f.l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.s.dir, "tasks.db")
	prepareCapacityFixture(t, "index", path, f.task.ID, f.run.ID)
	before, err := os.Stat(path)
	if err != nil || before.Size() >= MaxDatabaseBytes {
		t.Fatalf("invalid fixture size=%v err=%v", before, err)
	}
	t.Logf("index fixture: %d bytes", before.Size())
	migrated, err := Open(f.s.dir)
	if err != nil {
		t.Fatalf("index migration prevented opening existing data: %v", err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.s.dir)
	if reopened != nil {
		defer reopened.Close()
	}
	if err != nil || after.Size() > MaxDatabaseBytes {
		t.Fatalf("database no longer reopenable: before=%d after=%d err=%v", before.Size(), after.Size(), err)
	}
	if _, err := reopened.GetTask(t.Context(), f.task.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := reopened.db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name='tools_recovery'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("expected recovery index not to fit: count=%d error=%v", count, err)
	}
	if err := reopened.db.QueryRow("SELECT count(*) FROM tool_calls").Scan(&count); err != nil || count != 100000 {
		t.Fatalf("migration lost history: count=%d error=%v", count, err)
	}
}

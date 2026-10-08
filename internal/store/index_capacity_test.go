package store

import (
	"database/sql"
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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=DELETE; PRAGMA synchronous=OFF; DROP INDEX tools_recovery; DROP INDEX approvals_call;"); err != nil {
		t.Fatal(err)
	}
	// Populate ordinary schema-1 records, then pad to just below the real cap.
	_, err = db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<100000)
 INSERT INTO tool_calls(id,task_id,run_id,status,planned,record)
 SELECT printf('fixture-%08d',x),?,?,'succeeded','2026-10-08',
 json_object('id',printf('fixture-%08d',x),'task_id',?,'run_id',?,'name','read_file','arguments',json('{}'),'effect','read','status','succeeded','result',json('{"status":"succeeded"}')) FROM n`, f.task.ID, f.run.ID, f.task.ID, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE migration_padding(bytes BLOB)"); err != nil {
		t.Fatal(err)
	}
	for {
		var pages, pageSize int64
		if err := db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
			t.Fatal(err)
		}
		if pages*pageSize > MaxDatabaseBytes-2*(1<<20) {
			break
		}
		if _, err := db.Exec("INSERT INTO migration_padding VALUES(zeroblob(1048576))"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil || before.Size() >= MaxDatabaseBytes {
		t.Fatalf("invalid fixture size=%v err=%v", before, err)
	}
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
	if err := reopened.db.QueryRow("SELECT count(*) FROM tool_calls").Scan(&count); err != nil || count != 100000 {
		t.Fatalf("migration lost history: count=%d error=%v", count, err)
	}
}

//go:build !race

package store

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// These tests build ~256MB single-goroutine fixtures that race instrumentation
// makes ~15x slower; the non-race coverage run still executes them.

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

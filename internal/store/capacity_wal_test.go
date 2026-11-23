package store

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func prepareCapacityFixture(t *testing.T, mode, path string, ids ...string) {
	t.Helper()
	args := append([]string{"testdata/capacity.py", mode, path, strconv.Itoa(MaxDatabaseBytes)}, ids...)
	if output, err := exec.CommandContext(t.Context(), "python3", args...).CombinedOutput(); err != nil {
		t.Fatalf("prepare %s fixture: %s %v", mode, output, err)
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
	prepareCapacityFixture(t, "wal", path)
	info, err := os.Stat(path + "-wal")
	if err != nil || info.Size() <= MaxDatabaseBytes {
		t.Fatalf("WAL fixture: %v %v", info, err)
	}
	t.Logf("WAL fixture: %d bytes", info.Size())
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

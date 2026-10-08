// Package store persists foreground tasks in a local SQLite database. Reads
// never start execution or acquire workspace ownership.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"meldra/internal/task"
	_ "modernc.org/sqlite"
)

const (
	MaxArgumentsBytes     = 1 << 20
	MaxOutputBytes        = 256 << 10
	MaxEventBytes         = 64 << 10
	MaxArtifactBytes      = 16 << 20
	MaxDatabaseBytes      = 256 << 20
	MaxArtifactTotalBytes = 256 << 20
	maxTextBytes          = 64 << 10
)

type Store struct {
	db          *sql.DB
	dir         string
	lockDir     string
	dirIdentity string
	// beforeCommit is used only by package fault-injection tests. Production
	// commits always use the normal SQLite transaction path.
	beforeCommit func() error
}

// Open creates a private local store and migrates older schemas transactionally.
// It does not recover or execute any previously recorded work.
func Open(dir string) (*Store, error) {
	dir, err := privateRoot(dir)
	if err != nil {
		return nil, err
	}
	if err := secureDir(dir); err != nil {
		return nil, err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	dirIdentity, err := directoryIdentity(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "tasks.db")
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := secureFile(candidate); err != nil {
			return nil, err
		}
	}
	if err := secureExistingFile(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	// Acquire SQLite's write reservation before reading mutable state, avoiding
	// deferred-transaction snapshot races between different task processes.
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, dir: dir, dirIdentity: dirIdentity}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error      { return s.db.Close() }
func (s *Store) Directory() string { return s.dir }

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > task.SchemaVersion {
		return fmt.Errorf("task database schema %d is newer than supported schema %d", version, task.SchemaVersion)
	}
	if version == 0 {
		_, err = tx.ExecContext(ctx, `
CREATE TABLE tasks (id TEXT PRIMARY KEY, workspace TEXT NOT NULL, status TEXT NOT NULL, updated TEXT NOT NULL, record BLOB NOT NULL);
CREATE TABLE runs (id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), status TEXT NOT NULL, started TEXT NOT NULL, record BLOB NOT NULL);
CREATE INDEX runs_task ON runs(task_id, started);
CREATE UNIQUE INDEX one_active_run ON runs(task_id) WHERE status IN ('running','waiting_approval');
CREATE TABLE tool_calls (id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), run_id TEXT NOT NULL REFERENCES runs(id), status TEXT NOT NULL, planned TEXT NOT NULL, record BLOB NOT NULL);
CREATE INDEX tools_task ON tool_calls(task_id, planned);
CREATE TABLE approvals (id TEXT PRIMARY KEY, task_id TEXT NOT NULL REFERENCES tasks(id), run_id TEXT NOT NULL REFERENCES runs(id), call_id TEXT NOT NULL REFERENCES tool_calls(id), decision TEXT NOT NULL, record BLOB NOT NULL);
CREATE TABLE events (task_id TEXT NOT NULL REFERENCES tasks(id), seq INTEGER NOT NULL, record BLOB NOT NULL, PRIMARY KEY(task_id,seq));
CREATE TABLE legacy_imports (source_id TEXT NOT NULL, content_hash TEXT NOT NULL, task_id TEXT NOT NULL REFERENCES tasks(id), PRIMARY KEY(source_id,content_hash));
PRAGMA user_version = 1;`)
		if err != nil {
			return fmt.Errorf("migrate task database: %w", err)
		}
	}
	// This rebuildable index is backward compatible with schema 1. Keeping
	// provider identity indexed prevents replay lookup from decoding/scanning
	// the complete history on every subsequent tool call.
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS tools_provider_call ON tool_calls(task_id,json_extract(CAST(record AS TEXT),'$.provider_call_id'),planned,id)`); err != nil {
		return fmt.Errorf("index tool identities: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS tools_recovery ON tool_calls(task_id,status,id); CREATE INDEX IF NOT EXISTS approvals_call ON approvals(call_id)`); err != nil {
		return err
	}
	// Do not change newer databases at all. WAL is enabled only after checking
	// their version, and FULL sync protects committed state across process exit.
	var pageSize int
	if err := s.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		return err
	}
	if pageSize <= 0 {
		return errors.New("invalid SQLite page size")
	}
	for _, pragma := range []string{"PRAGMA journal_mode=WAL", "PRAGMA synchronous=FULL", "PRAGMA wal_autocheckpoint=256", "PRAGMA journal_size_limit=4194304", fmt.Sprintf("PRAGMA max_page_count=%d", MaxDatabaseBytes/pageSize)} {
		if _, err := s.db.ExecContext(ctx, pragma); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) transact(ctx context.Context, fn func(*sql.Tx) error) error {
	if identity, err := directoryIdentity(s.dir); err != nil || identity != s.dirIdentity {
		return fmt.Errorf("%w: task storage directory changed", task.ErrLease)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	if s.beforeCommit != nil {
		if err := s.beforeCommit(); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func NewID() string {
	var data [16]byte
	_, _ = rand.Read(data[:])
	return hex.EncodeToString(data[:])
}

func Hash(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func CanonicalWorkspace(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("workspace unavailable: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory: %s", path)
	}
	return path, nil
}

func validID(id string) bool {
	if id == "" || len(id) > 160 {
		return false
	}
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return false
		}
	}
	return id != "." && id != ".."
}

func validateText(text string, limit int) error {
	if len(text) > limit {
		return task.ErrLimit
	}
	return nil
}

func validateJSON(data json.RawMessage, limit int) error {
	if len(data) > limit {
		return task.ErrLimit
	}
	if len(data) > 0 && !json.Valid(data) {
		return errors.New("invalid JSON record")
	}
	return nil
}

func encode(value any) ([]byte, error) { return json.Marshal(value) }
func now() time.Time                   { return time.Now().UTC() }
func timestamp(t time.Time) string     { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func getRecord[T any](ctx context.Context, q queryer, query string, args ...any) (T, error) {
	var out T
	var raw []byte
	if err := q.QueryRowContext(ctx, query, args...).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, task.ErrNotFound
		}
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("corrupt task record: %w", err)
	}
	return out, nil
}

func secureDir(path string) error {
	if err := rejectSymlinkAncestors(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("store directory must be a real directory: %s", path)
	}
	return os.Chmod(path, 0700)
}

func rejectSymlinkAncestors(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for path != "/" && path != "." {
		info, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			// macOS exposes system-owned aliases for its private temp hierarchy.
			if !(runtime.GOOS == "darwin" && (path == "/var" || path == "/tmp" || path == "/etc")) {
				return fmt.Errorf("symlink in private storage path: %s", path)
			}
		}
		path = filepath.Dir(path)
	}
	return nil
}

func secureExistingFile(path string) error {
	if err := secureFile(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() > MaxDatabaseBytes {
		return task.ErrLimit
	}
	return nil
}

// WAL size is independent of the page limit; let SQLite recover its journal.
func secureFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("storage file must be regular: %s", path)
	}
	return os.Chmod(path, 0600)
}

// privateRoot resolves external ancestors, never the private root itself.
// All private descendants still pass the strict symlink checks.
func privateRoot(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(absolute)
	var missing []string
	for {
		_, err := os.Lstat(parent)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		missing = append(missing, filepath.Base(parent))
		parent = filepath.Dir(parent)
	}
	parent, err = filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		parent = filepath.Join(parent, missing[i])
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func (s *Store) appendEvent(ctx context.Context, tx *sql.Tx, event task.Event) error {
	if !validID(event.TaskID) || event.Kind == "" {
		return errors.New("event requires task ID and kind")
	}
	if err := validateJSON(event.Data, MaxEventBytes); err != nil {
		return err
	}
	if len(event.Kind) > 128 || len(event.Status) > 128 || len(event.Reason) > maxTextBytes {
		return task.ErrLimit
	}
	if event.RunID != "" {
		r, err := getRecord[task.Run](ctx, tx, "SELECT record FROM runs WHERE id=?", event.RunID)
		if err != nil {
			return err
		}
		if r.TaskID != event.TaskID {
			return errors.New("event run belongs to another task")
		}
	}
	if event.ToolCallID != "" {
		c, err := getRecord[task.ToolCall](ctx, tx, "SELECT record FROM tool_calls WHERE id=?", event.ToolCallID)
		if err != nil {
			return err
		}
		if c.TaskID != event.TaskID || event.RunID != "" && c.RunID != event.RunID {
			return errors.New("event tool belongs to another task or run")
		}
	}
	event.SchemaVersion = task.SchemaVersion
	event.ID = NewID()
	event.Time = now()
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE task_id=?", event.TaskID).Scan(&event.Sequence); err != nil {
		return err
	}
	raw, err := encode(event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO events(task_id,seq,record) VALUES(?,?,?)", event.TaskID, event.Sequence, raw)
	return err
}

// AppendEvent requires current execution ownership. Callers must not place
// secrets or provider reasoning in event data.
func (s *Store) AppendEvent(ctx context.Context, lease *Lease, event task.Event) error {
	return s.owned(ctx, lease, event.TaskID, func(tx *sql.Tx) error { return s.appendEvent(ctx, tx, event) })
}

func cleanProvider(provider string) bool {
	return !strings.ContainsAny(provider, "\r\n\x00") && len(provider) <= 4096
}

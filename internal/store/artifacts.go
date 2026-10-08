package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"meldra/internal/task"
)

// PutArtifact stores bounded content by its digest in a private directory.
// Artifacts contain full tool output, so callers must exclude secrets explicitly.
func (s *Store) PutArtifact(ctx context.Context, l *Lease, taskID, name string, data []byte) (task.ArtifactRef, error) {
	ref := task.ArtifactRef{ID: Hash(data), Name: name, SHA256: Hash(data), Size: int64(len(data))}
	if name == "" || len(name) > 256 || len(data) > MaxArtifactBytes {
		return ref, task.ErrArtifactLimit
	}
	created := false
	err := s.owned(ctx, l, taskID, func(tx *sql.Tx) error {
		dir := filepath.Join(s.dir, "artifacts")
		if err := secureDir(dir); err != nil {
			return err
		}
		path := filepath.Join(dir, ref.ID)
		if err := secureExistingFile(path); err != nil {
			return err
		}
		if info, err := os.Stat(path); err == nil && info.Size() > MaxArtifactBytes {
			return task.ErrArtifactLimit
		}
		if existing, err := os.ReadFile(path); err == nil {
			if Hash(existing) != ref.SHA256 {
				return errors.New("artifact digest mismatch")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		} else {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return err
			}
			var total int64
			for _, entry := range entries {
				// The immediate write transaction excludes every other artifact writer.
				// Temporary files can only be leftovers from a terminated writer here.
				if strings.HasPrefix(entry.Name(), ".pending-") {
					if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
						return err
					}
					continue
				}
				info, err := entry.Info()
				if err != nil {
					return err
				}
				total += info.Size()
			}
			if total+ref.Size > MaxArtifactTotalBytes {
				// Reclaim only digest files with no committed save event or tool
				// reference. The immediate transaction excludes concurrent writers.
				for _, entry := range entries {
					if !validDigest(entry.Name()) {
						continue
					}
					referenced, err := artifactReferenced(ctx, tx, entry.Name())
					if err != nil {
						return err
					}
					if referenced {
						continue
					}
					info, err := entry.Info()
					if err != nil {
						return err
					}
					if !info.Mode().IsRegular() {
						continue
					}
					if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
						return err
					}
					total -= info.Size()
				}
				if total+ref.Size > MaxArtifactTotalBytes {
					return task.ErrArtifactLimit
				}
			}
			f, err := os.CreateTemp(dir, ".pending-")
			if err != nil {
				return err
			}
			defer os.Remove(f.Name())
			if _, err := f.Write(data); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Sync(); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if err := os.Rename(f.Name(), path); err != nil {
				return err
			}
			created = true
			d, err := os.Open(dir)
			if err != nil {
				return err
			}
			err = errors.Join(d.Sync(), d.Close())
			if err != nil {
				return err
			}
		}
		raw, _ := json.Marshal(ref)
		return s.appendEvent(ctx, tx, task.Event{TaskID: taskID, Kind: "artifact.saved", Data: raw})
	})
	if err != nil && created {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cleanupErr := s.owned(cleanupCtx, l, taskID, func(tx *sql.Tx) error {
			referenced, checkErr := artifactReferenced(cleanupCtx, tx, ref.ID)
			if checkErr != nil || referenced {
				return checkErr
			}
			path := filepath.Join(s.dir, "artifacts", ref.ID)
			if checkErr := rejectSymlinkAncestors(path); checkErr != nil {
				return checkErr
			}
			removeErr := os.Remove(path)
			if errors.Is(removeErr, os.ErrNotExist) {
				return nil
			}
			return removeErr
		})
		err = errors.Join(err, cleanupErr)
	}
	return ref, err
}

func artifactReferenced(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var found bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE json_extract(CAST(record AS TEXT),'$.kind')='artifact.saved' AND json_extract(CAST(record AS TEXT),'$.data.id')=?) OR EXISTS(SELECT 1 FROM tool_calls, json_each(CAST(tool_calls.record AS TEXT),'$.result.artifacts') AS artifact WHERE json_extract(artifact.value,'$.id')=?)`, id, id).Scan(&found)
	return found, err
}

func (s *Store) ReadArtifact(ctx context.Context, ref task.ArtifactRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validDigest(ref.ID) || ref.ID != ref.SHA256 || ref.Size < 0 || ref.Size > MaxArtifactBytes {
		return nil, errors.New("invalid artifact reference")
	}
	path := filepath.Join(s.dir, "artifacts", ref.ID)
	if err := rejectSymlinkAncestors(path); err != nil {
		return nil, err
	}
	if err := secureExistingFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.Size || Hash(data) != ref.SHA256 {
		return nil, errors.New("artifact content or size does not match digest")
	}
	return data, nil
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

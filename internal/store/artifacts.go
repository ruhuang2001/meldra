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

	"meldra/internal/task"
)

// PutArtifact stores bounded content by its digest in a private directory.
// Artifacts contain full tool output, so callers must exclude secrets explicitly.
func (s *Store) PutArtifact(ctx context.Context, l *Lease, taskID, name string, data []byte) (task.ArtifactRef, error) {
	ref := task.ArtifactRef{ID: Hash(data), Name: name, SHA256: Hash(data), Size: int64(len(data))}
	if name == "" || len(name) > 256 || len(data) > MaxArtifactBytes {
		return ref, task.ErrLimit
	}
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
			return task.ErrLimit
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
				info, err := entry.Info()
				if err != nil {
					return err
				}
				total += info.Size()
			}
			if total+ref.Size > MaxArtifactTotalBytes {
				return task.ErrLimit
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
	return ref, err
}

func (s *Store) ReadArtifact(ctx context.Context, ref task.ArtifactRef) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(ref.ID) != 64 || ref.ID != ref.SHA256 || strings.ContainsAny(ref.ID, "/\\.") || ref.Size < 0 || ref.Size > MaxArtifactBytes {
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

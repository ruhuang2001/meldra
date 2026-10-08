package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Fingerprints record evidence about files, not their contents. Recovery only
// recognizes a complete before/after state; mixed results require human review.
type fileFingerprint struct {
	Path             string                 `json:"path"`
	BeforeExists     bool                   `json:"before_exists"`
	AfterExists      bool                   `json:"after_exists"`
	BeforeHash       string                 `json:"before_hash"`
	AfterHash        string                 `json:"after_hash"`
	Mode             fs.FileMode            `json:"mode"`
	AuxiliaryVersion int                    `json:"auxiliary_version,omitzero"`
	Directories      []directoryFingerprint `json:"directories,omitempty"`
}

type directoryFingerprint struct {
	Path         string `json:"path"`
	BeforeExists bool   `json:"before_exists"`
	AfterExists  bool   `json:"after_exists"`
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func changeFingerprints(changes []fileChange, reverse bool) json.RawMessage {
	records := make([]fileFingerprint, 0, len(changes))
	for _, change := range changes {
		record := fileFingerprint{Path: change.path, BeforeExists: change.existed, AfterExists: change.afterExists, BeforeHash: digest(change.before), AfterHash: digest(change.after), Mode: change.mode}
		record.AuxiliaryVersion = 1
		for dir := filepath.Dir(change.path); ; dir = filepath.Dir(dir) {
			_, err := os.Stat(dir)
			if err == nil {
				break
			}
			if !os.IsNotExist(err) {
				record.AuxiliaryVersion = 0
				break
			}
			record.Directories = append(record.Directories, directoryFingerprint{Path: dir, AfterExists: true})
			if filepath.Dir(dir) == dir {
				record.AuxiliaryVersion = 0
				break
			}
		}
		if reverse {
			record.BeforeExists, record.AfterExists = record.AfterExists, record.BeforeExists
			record.BeforeHash, record.AfterHash = record.AfterHash, record.BeforeHash
			record.Directories = nil
			for _, dir := range change.createdDirs {
				record.Directories = append(record.Directories, directoryFingerprint{Path: dir, BeforeExists: true})
			}
		}
		records = append(records, record)
	}
	data, _ := json.Marshal(records)
	return data
}

func (w *Workspace) reconcileFiles(raw json.RawMessage) (string, error) {
	var records []fileFingerprint
	if err := json.Unmarshal(raw, &records); err != nil {
		return "", err
	}
	if len(records) == 0 {
		return "", fmt.Errorf("no file evidence")
	}
	before, after := true, true
	checkedParents := make(map[string]bool)
	for _, record := range records {
		// Older evidence cannot prove that auxiliary effects are absent.
		if record.AuxiliaryVersion != 1 {
			return "changed", nil
		}
		path, err := w.resolve(record.Path, true)
		if err != nil {
			return "", err
		}
		contents, exists, mode, err := readEditableFile(path)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		hash := digest(contents)
		before = before && exists == record.BeforeExists && (!exists || (hash == record.BeforeHash && mode == record.Mode))
		after = after && exists == record.AfterExists && (!exists || (hash == record.AfterHash && mode == record.Mode))
		for _, directory := range record.Directories {
			resolved, err := w.resolve(directory.Path, true)
			if err != nil {
				return "", err
			}
			info, err := os.Lstat(resolved)
			if err != nil && !os.IsNotExist(err) {
				return "", err
			}
			present := err == nil
			if present && !info.IsDir() {
				return "changed", nil
			}
			before = before && present == directory.BeforeExists
			after = after && present == directory.AfterExists
		}
		// Unowned leftovers are evidence of uncertainty, never permission to
		// delete workspace files. Scan in bounded batches, including legacy temps.
		parent := filepath.Dir(path)
		if !checkedParents[parent] {
			leftover, err := hasWriteTemporary(parent)
			if err != nil {
				return "", err
			}
			if leftover {
				return "changed", nil
			}
			checkedParents[parent] = true
		}
	}
	if after {
		return "after", nil
	}
	if before {
		return "before", nil
	}
	return "changed", nil
}

func hasWriteTemporary(dir string) (bool, error) {
	f, err := os.Open(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	for {
		entries, err := f.ReadDir(128)
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasPrefix(entry.Name(), ".meldra-write-") && strings.HasSuffix(entry.Name(), ".tmp") {
				return true, nil
			}
		}
		if err == io.EOF {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
}

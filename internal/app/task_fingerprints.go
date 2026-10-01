package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
)

// Fingerprints record evidence about files, not their contents. Recovery only
// recognizes a complete before/after state; mixed results require human review.
type fileFingerprint struct {
	Path         string      `json:"path"`
	BeforeExists bool        `json:"before_exists"`
	AfterExists  bool        `json:"after_exists"`
	BeforeHash   string      `json:"before_hash"`
	AfterHash    string      `json:"after_hash"`
	Mode         fs.FileMode `json:"mode"`
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func changeFingerprints(changes []fileChange, reverse bool) json.RawMessage {
	records := make([]fileFingerprint, 0, len(changes))
	for _, change := range changes {
		record := fileFingerprint{Path: change.path, BeforeExists: change.existed, AfterExists: change.afterExists, BeforeHash: digest(change.before), AfterHash: digest(change.after), Mode: change.mode}
		if reverse {
			record.BeforeExists, record.AfterExists = record.AfterExists, record.BeforeExists
			record.BeforeHash, record.AfterHash = record.AfterHash, record.BeforeHash
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
	for _, record := range records {
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
	}
	if after {
		return "after", nil
	}
	if before {
		return "before", nil
	}
	return "changed", nil
}

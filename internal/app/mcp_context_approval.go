package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

// mcpApprovalContextDigest runs under the server's operation lock. Resolve
// every actual edit target before minting or redeeming a multi-request approval.
// Programs have no known write set, but still bind root/active instructions.
func (w *Workspace) mcpApprovalContextDigest(name string, raw json.RawMessage) (string, error) {
	if w.projectContext == nil {
		return "", nil
	}
	var paths []string
	switch name {
	case "edit_file":
		var in struct {
			Path string `json:"path"`
			Old  string `json:"old_str"`
			New  string `json:"new_str"`
		}
		if err := decodeToolInput(raw, &in, "path", "old_str", "new_str"); err != nil {
			return "", err
		}
		paths = append(paths, in.Path)
	case "apply_patch":
		var in struct {
			Patch   string        `json:"patch"`
			Changes []changeInput `json:"changes"`
		}
		if err := decodeToolInput(raw, &in); err != nil {
			return "", err
		}
		if (strings.TrimSpace(in.Patch) == "") == (len(in.Changes) == 0) {
			return "", fmt.Errorf("set exactly one of patch or changes")
		}
		if in.Patch != "" {
			if len(in.Patch) > maxPatchBytes {
				return "", fmt.Errorf("patch exceeds the %d byte limit", maxPatchBytes)
			}
			files, err := parseUnifiedPatch(in.Patch)
			if err != nil {
				return "", err
			}
			for _, file := range files {
				path := file.newPath
				if path == "/dev/null" {
					path = file.oldPath
				}
				paths = append(paths, path)
			}
		} else {
			for _, change := range in.Changes {
				paths = append(paths, change.Path)
			}
		}
		if err := validateChangeCount(len(paths)); err != nil {
			return "", err
		}
	case "undo_last_change":
		if len(w.last) == 0 {
			return "", fmt.Errorf("no successful change to undo")
		}
		for _, change := range w.last {
			paths = append(paths, change.path)
		}
	}
	return w.projectContext.ApprovalDigest(paths)
}

//go:build darwin || linux

package main

import (
	"syscall"
	"testing"
)

func TestWorkspaceReadRejectsFIFOWithoutOpeningIt(t *testing.T) {
	root := t.TempDir()
	path := root + "/pipe"
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	w, _ := testWorkspace(t, root, "", true)
	if _, err := callTool(t, w, "read_file", map[string]any{"path": "pipe"}); err == nil {
		t.Fatal("read_file accepted a FIFO")
	}
}

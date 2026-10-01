//go:build darwin || linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"meldra/internal/tool"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestTaskCancellationStopsGitPreflight(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := gitTopLevel(ctx, t.TempDir(), executable)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("preflight error=%v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("Git preflight ignored task cancellation")
	}
}

func TestTaskVerificationPreservesFailureAndStopsUnknown(t *testing.T) {
	for _, mode := range []string{"failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			workspace, _ := testWorkspace(t, t.TempDir(), "", true)
			if err := os.WriteFile(filepath.Join(workspace.root, "go.mod"), []byte("module example.invalid/test\n\ngo 1.26\n"), 0600); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			marker := filepath.Join(bin, "calls")
			script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> " + strconv.Quote(marker) + "\n"
			if mode == "cancel" {
				script += "exec sleep 30\n"
			} else {
				script += "if [ \"$1\" = vet ]; then exit 7; fi\nexit 0\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx := t.Context()
			if mode == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
			}
			registry, err := tool.New(workspace.ToolDefinitions())
			if err != nil {
				t.Fatal(err)
			}
			result, err := registry.Invoke(ctx, "verify", json.RawMessage(`{"preset":"check"}`))
			if err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				if result.Status != tool.Unknown || string(calls) != "vet\n" {
					t.Fatalf("result=%+v calls=%q", result, calls)
				}
			} else {
				if result.Status != tool.Failed || result.ExitCode == nil || *result.ExitCode != 7 || string(calls) != "vet\ntest\n" {
					t.Fatalf("result=%+v calls=%q", result, calls)
				}
			}
		})
	}
}

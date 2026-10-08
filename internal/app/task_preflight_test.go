//go:build darwin || linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"meldra/internal/provider"
	"meldra/internal/task"
	"meldra/internal/tool"
)

func TestTaskNonGitPreflightFailureDoesNotBlockNextTool(t *testing.T) {
	for _, test := range []struct {
		name string
		args string
	}{
		{"verify", `{"preset":"diff"}`},
		{"run_command", `{"command":"git","args":["diff","--stat"],"timeout":60}`},
		{"git_review", `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			backend := inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
				requests++
				switch requests {
				case 1:
					return provider.Result{Response: callResponse("git-check", test.name, test.args)}, nil
				case 2:
					return provider.Result{Response: callResponse("continue-reading", "list_files", `{"path":"."}`)}, nil
				default:
					return provider.Result{Response: finishResponse()}, nil
				}
			})
			agent, paths := recordedAgent(t, backend, nil)
			if err := agent.RunTurn(t.Context(), "inspect this non-Git workspace"); err != nil {
				t.Fatal(err)
			}
			calls, err := openTaskDB(t, paths).ToolCalls(t.Context(), agent.session.ID)
			if err != nil || len(calls) != 2 || requests != 3 {
				t.Fatalf("calls=%+v requests=%d err=%v", calls, requests, err)
			}
			if calls[0].Status != task.ToolFailed || !strings.Contains(calls[0].Result.Error, "not a git repository") || calls[1].Status != task.ToolSucceeded {
				t.Fatalf("preflight must fail without blocking the next tool: %+v", calls)
			}
		})
	}
}

func TestTaskCancelledGitPreflightHasKnownOutcome(t *testing.T) {
	bin := t.TempDir()
	ready := filepath.Join(bin, "ready")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\ntouch "+strconv.Quote(ready)+"\nexec sleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	registry, err := tool.New(w.ToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticks := time.Tick(5 * time.Millisecond)
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		for {
			select {
			case <-ticks:
				if _, err := os.Stat(ready); err == nil {
					cancel()
					return
				}
			case <-deadline.C:
				cancel()
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); <-done }()
	result, err := registry.Invoke(ctx, "git_review", json.RawMessage(`{}`))
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("Git preflight never started: %v", err)
	}
	if !errors.Is(err, context.Canceled) || result.Status != tool.Cancelled {
		t.Fatalf("read-only preflight cancellation=%+v err=%v", result, err)
	}
}

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
				ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
				stopped := make(chan struct{})
				go func() {
					defer close(stopped)
					poll := time.Tick(5 * time.Millisecond)
					for {
						select {
						case <-ctx.Done():
							return
						case <-poll:
							// Cancel only after the child has performed its recorded effect.
							if calls, err := os.ReadFile(marker); err == nil && string(calls) == "vet\n" {
								cancel()
								return
							}
						}
					}
				}()
				defer func() {
					cancel()
					<-stopped
				}()
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
				if !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatalf("child did not reach cancellation handshake: %v", ctx.Err())
				}
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

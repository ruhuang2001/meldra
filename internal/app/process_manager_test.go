package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
	"meldra/internal/tool"
)

func TestManagedProcessHelper(t *testing.T) {
	index := slices.Index(os.Args, "--managed-helper")
	if index < 0 {
		return
	}
	switch os.Args[index+1] {
	case "counted-wait", "counted-exit":
		file, err := os.OpenFile("spawn-count.txt", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			os.Exit(9)
		}
		_, _ = file.WriteString("spawned\n")
		_ = file.Close()
		fmt.Println("counted child ready")
		if os.Args[index+1] == "counted-exit" {
			os.Exit(0)
		}
		for {
			time.Sleep(time.Second)
		}
	case "exit":
		fmt.Print("hello\n\x1b[31mred\x1b[0m\n")
		os.Exit(7)
	case "wait":
		fmt.Println("ready")
		for {
			time.Sleep(time.Second)
		}
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		fmt.Println("ready")
		for {
			time.Sleep(time.Second)
		}
	case "cooperative-term":
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		fmt.Println("ready")
		<-signals
		fmt.Println("term received")
		os.Exit(0)
	case "environment":
		fmt.Printf("HOME=%s\n", os.Getenv("HOME"))
		for {
			if _, err := os.Stat(os.Getenv("HOME")); err != nil {
				fmt.Println("home lost")
				os.Exit(9)
			}
			time.Sleep(5 * time.Millisecond)
		}
	case "burst":
		chunk := bytes.Repeat([]byte("a"), 64<<10)
		for range 300 {
			_, _ = os.Stdout.Write(chunk)
		}
		os.Exit(0)
	case "burst-wait":
		chunk := bytes.Repeat([]byte("a"), 64<<10)
		for range 300 {
			_, _ = os.Stdout.Write(chunk)
		}
		fmt.Println("ready")
		for {
			time.Sleep(time.Second)
		}
	case "pipe-child":
		child := exec.Command(os.Args[0], "-test.run=^TestManagedProcessHelper$", "--", "--managed-helper", "wait")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(9)
		}
		os.Exit(0)
	}
}

type processView struct {
	Process   task.Process `json:"process"`
	Output    string       `json:"output"`
	Cursor    int64        `json:"cursor"`
	Truncated bool         `json:"truncated"`
}

func managedWorkspace(t *testing.T) (*Workspace, *tool.Registry, string) {
	t.Helper()
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	w.policy, _ = newRuntimePolicy(ModeBuild, PermissionInteractive)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetCommandExecutables([]string{executable}); err != nil {
		t.Fatal(err)
	}
	if err := w.beginProcesses(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.closeProcesses() })
	definitions := append(w.ToolDefinitions(), w.ProcessToolDefinitions()...)
	registry, err := tool.New(definitions)
	if err != nil {
		t.Fatal(err)
	}
	return w, registry, executable
}

func invokeProcess(t *testing.T, registry *tool.Registry, name string, input any) processView {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Invoke(t.Context(), name, raw)
	if err != nil {
		t.Fatal(err)
	}
	var view processView
	if err := json.Unmarshal([]byte(result.Output), &view); err != nil {
		t.Fatalf("decode process: %q %v", result.Output, err)
	}
	return view
}

func helperInput(executable, mode string, timeout int) map[string]any {
	return map[string]any{"command": executable, "args": []string{"-test.run=^TestManagedProcessHelper$", "--", "--managed-helper", mode}, "timeout": timeout}
}

func awaitProcess(t *testing.T, registry *tool.Registry, id string) processView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		view := invokeProcess(t, registry, "wait_process", map[string]any{"process_id": id, "cursor": 0, "wait_ms": 100})
		if view.Process.State.Terminal() {
			return view
		}
	}
	t.Fatal("process did not finish")
	return processView{}
}

func TestManagedProcessExitOutputCursorAndRunIsolation(t *testing.T) {
	w, registry, executable := managedWorkspace(t)
	view := invokeProcess(t, registry, "start_process", helperInput(executable, "exit", 10))
	view = awaitProcess(t, registry, view.Process.ID)
	if view.Process.State != task.ProcessExited || view.Process.Effects != "known" || view.Process.ExitCode == nil || *view.Process.ExitCode != 7 || !strings.Contains(view.Output, "hello\nred") || strings.ContainsRune(view.Output, '\x1b') {
		t.Fatalf("invalid exit: %+v", view)
	}
	next := invokeProcess(t, registry, "process_status", map[string]any{"process_id": view.Process.ID, "cursor": view.Cursor})
	if next.Output != "" || next.Cursor != view.Cursor {
		t.Fatalf("cursor repeated output: %+v", next)
	}
	if err := w.closeProcesses(); err != nil {
		t.Fatal(err)
	}
	if err := w.beginProcesses(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"process_id": view.Process.ID})
	if _, err := registry.Invoke(t.Context(), "process_status", raw); err == nil {
		t.Fatal("previous Run handle accepted")
	}
	for _, def := range w.ToolDefinitions() {
		if strings.Contains(def.Name, "process") {
			t.Fatal("process tool exported to stateless MCP catalog")
		}
	}
}

func TestManagedProcessObservationCancellationAndWriterExclusion(t *testing.T) {
	w, registry, executable := managedWorkspace(t)
	view := invokeProcess(t, registry, "start_process", helperInput(executable, "environment", 10))
	observe, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	raw, _ := json.Marshal(map[string]any{"process_id": view.Process.ID, "wait_ms": 10000})
	if _, err := registry.Invoke(observe, "wait_process", raw); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("observation did not cancel: %v", err)
	}
	current := invokeProcess(t, registry, "process_status", map[string]any{"process_id": view.Process.ID})
	if current.Process.State != task.ProcessRunning {
		t.Fatalf("poll context killed child: %+v", current)
	}
	edit := json.RawMessage(`{"path":"bad.txt","old_str":"","new_str":"bad"}`)
	if _, err := registry.Invoke(t.Context(), "edit_file", edit); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("parallel writer admitted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(w.root, "bad.txt")); !os.IsNotExist(err) {
		t.Fatal("parallel edit wrote file")
	}
	launch, _ := json.Marshal(helperInput(executable, "exit", 10))
	if _, err := registry.Invoke(t.Context(), "start_process", launch); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("second command admitted: %v", err)
	}
	stop := invokeProcess(t, registry, "stop_process", map[string]any{"process_id": view.Process.ID})
	if stop.Process.State != task.ProcessStopped || stop.Process.Effects != "unknown" {
		t.Fatalf("stop invented effect success: %+v", stop)
	}
	again := invokeProcess(t, registry, "stop_process", map[string]any{"process_id": view.Process.ID})
	if again.Process.EndedAt != stop.Process.EndedAt {
		t.Fatal("repeat stop changed process outcome")
	}
	if err := w.closeProcesses(); !errors.Is(err, task.ErrUnresolved) {
		t.Fatalf("unknown effects accepted: %v", err)
	}
}

func TestManagedProcessTimeoutAndBoundedOutput(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		_, registry, executable := managedWorkspace(t)
		view := invokeProcess(t, registry, "start_process", helperInput(executable, "wait", 1))
		view = awaitProcess(t, registry, view.Process.ID)
		if view.Process.State != task.ProcessTimedOut || view.Process.Effects != "unknown" {
			t.Fatalf("bad timeout: %+v", view)
		}
	})
	t.Run("burst", func(t *testing.T) {
		w, registry, executable := managedWorkspace(t)
		view := invokeProcess(t, registry, "start_process", helperInput(executable, "burst", 10))
		view = awaitProcess(t, registry, view.Process.ID)
		if view.Process.State != task.ProcessExited || view.Process.ExitCode == nil || *view.Process.ExitCode != 0 || !view.Truncated || !view.Process.Truncated || len(view.Output) > 64<<10 {
			t.Fatalf("output was unbounded or child blocked: %+v", view.Process)
		}
		p := w.processes.processes[view.Process.ID]
		p.log.mu.Lock()
		retained, tail, total := p.log.retained, len(p.log.tail), p.log.total
		p.log.mu.Unlock()
		wantTotal := int64(300 * (64 << 10))
		if testing.CoverMode() != "" {
			// The child is this coverage-instrumented test binary. The production
			// environment deliberately excludes GOCOVERDIR; Go emits this exact
			// startup diagnostic before the fixture writes its payload. Count it
			// explicitly rather than loosening the complete-drain assertion.
			const diagnostic = "warning: GOCOVERDIR not set, no coverage data emitted\n"
			wantTotal += int64(len(diagnostic))

		}
		if retained != commandLogLimit || tail != maxToolOutput || total != wantTotal {
			t.Fatalf("retention=%d tail=%d total=%d", retained, tail, total)
		}
	})
}

func TestManagedProcessInheritedPipeCleanup(t *testing.T) {
	_, registry, executable := managedWorkspace(t)
	start := time.Now()
	view := invokeProcess(t, registry, "start_process", helperInput(executable, "pipe-child", 10))
	view = awaitProcess(t, registry, view.Process.ID)
	if time.Since(start) > 5*time.Second || view.Process.Effects != "unknown" {
		t.Fatalf("unbounded or falsely known inherited pipe: %+v", view)
	}
}

func TestCommandLiveOutputAndConfiguredDeadline(t *testing.T) {
	w, registry, executable := managedWorkspace(t)
	if err := w.SetCommandTimeout(240); err != nil {
		t.Fatal(err)
	}
	spec, err := w.prepareCommand(executable, []string{"-test.run=^TestManagedProcessHelper$", "--", "--managed-helper", "exit"}, 0)
	if err != nil || spec.TimeoutSeconds != 240 {
		t.Fatalf("deadline=%+v err=%v", spec, err)
	}
	seen := make(chan CommandOutput, 1)
	w.SetCommandOutput(func(output CommandOutput) {
		select {
		case seen <- output:
		default:
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		raw, _ := json.Marshal(helperInput(executable, "wait", 10))
		result, err := registry.Invoke(ctx, "run_command", raw)
		if err == nil && (result.Status != tool.Unknown || !strings.Contains(result.Output, "cancelled")) {
			err = fmt.Errorf("unexpected result: %+v", result)
		}
		done <- err
	}()
	select {
	case output := <-seen:
		if !strings.Contains(output.Text, "ready") {
			t.Fatal(output)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no live output before exit")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("command did not stop")
	}
}

func TestCommandLogSpoolFailureStillDrains(t *testing.T) {
	log, err := newCommandLog(nil, "")
	if err != nil {
		t.Fatal(err)
	}
	defer log.close()
	_ = log.file.Close()
	n, err := io.Copy(log, strings.NewReader(strings.Repeat("x", maxToolOutput*2)))
	if err != nil || n != maxToolOutput*2 {
		t.Fatalf("pipe drainage stopped: %d %v", n, err)
	}
	if !log.truncated || len(log.tail) > maxToolOutput {
		t.Fatal("spool failure not recorded")
	}
}

func TestManagedProcessDurableCleanupBeforeLeaseRelease(t *testing.T) {
	agent, paths := recordedAgent(t, nil, nil)
	e := agent.execution
	w := e.workspace
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.SetCommandExecutables([]string{executable}); err != nil {
		t.Fatal(err)
	}
	if err := e.begin(t.Context(), "run foreground server"); err != nil {
		t.Fatal(err)
	}
	if err := w.beginProcesses(t.Context(), e); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.closeProcesses(); _ = e.close() })
	registry, err := tool.New(w.ProcessToolDefinitions())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(helperInput(executable, "wait", 10))
	result, err := e.invoke(t.Context(), registry, "start-one", "start_process", raw)
	if err != nil || result.Status != tool.Succeeded {
		t.Fatalf("start=%+v %v", result, err)
	}
	var view processView
	if err := json.Unmarshal([]byte(result.Output), &view); err != nil {
		t.Fatal(err)
	}
	replayed, err := e.invoke(t.Context(), registry, "start-one", "start_process", raw)
	if err != nil || replayed.Output != result.Output || len(w.processes.processes) != 1 {
		t.Fatalf("duplicate start call launched again: %+v %v", replayed, err)
	}
	observer, err := taskstore.Open(taskDirectory(paths))
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	if lease, err := observer.Acquire(t.Context(), taskstore.NewID(), w.root); !errors.Is(err, task.ErrBusy) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatalf("lease released while child active: %v", err)
	}
	cleanupErr := w.closeProcesses()
	if !errors.Is(cleanupErr, task.ErrUnresolved) {
		t.Fatalf("stopped process omitted unknown effects: %v", cleanupErr)
	}
	record, err := observer.GetProcess(t.Context(), view.Process.ID)
	if err != nil || record.State != task.ProcessStopped || record.Effects != "unknown" || record.EndedAt.IsZero() {
		t.Fatalf("cleanup outcome not durable: %+v %v", record, err)
	}
	if lease, err := observer.Acquire(t.Context(), taskstore.NewID(), w.root); !errors.Is(err, task.ErrBusy) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatalf("manager cleanup released Run ownership: %v", err)
	}
	if err := e.finish(t.Context(), cleanupErr); err != nil {
		t.Fatal(err)
	}
	lease, err := observer.Acquire(t.Context(), taskstore.NewID(), w.root)
	if err != nil {
		t.Fatalf("lease not released after durable cleanup: %v", err)
	}
	_ = lease.Close()
	calls, err := observer.ToolCalls(t.Context(), agent.session.ID)
	if err != nil || len(calls) != 1 || calls[0].Status != task.ToolSucceeded {
		t.Fatalf("cleanup rewrote successful launch as command result: %+v %v", calls, err)
	}
}

func TestManagedProcessApprovalFailureNeverSpawns(t *testing.T) {
	for _, failure := range []string{"declined", "recording", "mode"} {
		t.Run(failure, func(t *testing.T) {
			w, registry, executable := managedWorkspace(t)
			w.autoApprove = false
			w.SetApprovalFunc(func(context.Context, ApprovalRequest) bool {
				if failure == "mode" {
					_ = w.policy.setMode(ModePlan)
				}
				return failure != "declined"
			})
			if failure == "recording" {
				w.approvalRecord = func(context.Context, ApprovalRequest, bool) error { return errors.New("disk failure") }
			}
			raw, _ := json.Marshal(helperInput(executable, "wait", 10))
			_, _ = registry.Invoke(t.Context(), "start_process", raw)
			if len(w.processes.processes) != 0 {
				t.Fatal("failed approval spawned process")
			}
		})
	}
}

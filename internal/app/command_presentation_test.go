//go:build darwin || linux

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/charmbracelet/x/term"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"meldra/internal/provider"
	taskstore "meldra/internal/store"
	"meldra/internal/task"
)

func unreadOutput(t *testing.T, ctx context.Context) (*synchronizedWriter, *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	bounded, cleanup, err := newSynchronizedWriter(ctx, writer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	return bounded, reader
}

func TestSynchronizedWriterBoundsUnreadPipeAndKeepsFailure(t *testing.T) {
	writer, _ := unreadOutput(t, t.Context())
	started := time.Now()
	n, err := writer.Write([]byte(strings.Repeat("x", 4<<20)))
	if err == nil || n == 4<<20 || time.Since(started) > 3*time.Second {
		t.Fatalf("unbounded output n=%d err=%v elapsed=%s", n, err, time.Since(started))
	}
	started = time.Now()
	if _, err := writer.Write([]byte("late result")); err == nil || time.Since(started) > 100*time.Millisecond {
		t.Fatalf("sticky output failure retried: %v", err)
	}
	if writer.Err() == nil {
		t.Fatal("output failure was hidden")
	}
}

func TestSynchronizedWriterCancellationInterruptsBlockedWrite(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer, _ := unreadOutput(t, ctx)
	done := make(chan error, 1)
	go func() { _, err := writer.Write([]byte(strings.Repeat("x", 4<<20))); done <- err }()
	time.Sleep(30 * time.Millisecond)
	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled write succeeded")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancel did not interrupt output")
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("slow cancellation")
	}
}

func TestSynchronizedWriterDescriptorKeepsDeadlinePolling(t *testing.T) {
	var _ term.File = (*synchronizedWriter)(nil)
	writer, _ := unreadOutput(t, t.Context())
	for range 10 {
		fd := writer.Fd()
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
		if errno != 0 || flags&syscall.O_NONBLOCK == 0 {
			t.Fatalf("terminal descriptor lookup removed nonblocking IO: flags=%x err=%v", flags, errno)
		}
	}
	started := time.Now()
	if _, err := writer.Write([]byte(strings.Repeat("x", 1<<20))); err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("descriptor lookup disabled write deadline: %v", err)
	}
	if (&synchronizedWriter{writer: io.Discard}).Fd() != ^uintptr(0) {
		t.Fatal("non-file writer exposed a terminal descriptor")
	}
}

type nonCancelableOutput struct{ writes int }

func (w *nonCancelableOutput) Write(data []byte) (int, error) { w.writes++; return len(data), nil }

func TestCommandPresentationDoesNotSpawnForArbitraryWriter(t *testing.T) {
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	out := &nonCancelableOutput{}
	a := &Agent{workspace: w, output: out}
	stop := a.startCommandPresentation(t.Context())
	if w.commandOutput != nil {
		t.Fatal("uncancelable writer received a live worker")
	}
	stop()
	if out.writes != 0 {
		t.Fatal("background writer was called")
	}
}

func TestCommandPresentationDeliversLiveMarkerToDrainingPipe(t *testing.T) {
	writer, reader := unreadOutput(t, t.Context())
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	a := &Agent{workspace: w, output: writer}
	stop := a.startCommandPresentation(t.Context())
	defer stop()
	if w.commandOutput == nil {
		t.Fatal("bounded pipe did not enable live output")
	}
	const marker = "live marker before exit\n"
	done := make(chan string, 1)
	go func() { data := make([]byte, len(marker)); _, _ = io.ReadFull(reader, data); done <- string(data) }()
	w.commandOutput(CommandOutput{Text: marker, Cursor: int64(len(marker))})
	select {
	case got := <-done:
		if got != marker {
			t.Fatalf("live output=%q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("live marker not delivered before command completion")
	}
}

func TestUnreadLiveOutputDoesNotRetainRunLeaseAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer, _ := unreadOutput(t, ctx)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launch, _ := json.Marshal(helperInput(executable, "burst-wait", 30))
	started := make(chan struct{})
	requests := 0
	agent, paths := recordedAgent(t, inferenceFunc(func(ctx context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		requests++
		if requests == 1 {
			return provider.Result{Response: callResponse("start", "start_process", string(launch))}, nil
		}
		close(started)
		<-ctx.Done()
		return provider.Result{}, ctx.Err()
	}), nil)
	w := agent.execution.workspace
	if err := w.SetCommandExecutables([]string{executable}); err != nil {
		t.Fatal(err)
	}
	agent.workspace, agent.output = w, writer
	agent.tools = append(w.ToolDefinitions(), w.ProcessToolDefinitions()...)
	done := make(chan error, 1)
	go func() { done <- agent.RunTurn(ctx, "start a foreground process") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not launch")
	}
	// The child produces more output than a pipe can retain. The reader stays
	// open and deliberately unread, modeling a stalled consumer rather than EPIPE.
	time.Sleep(100 * time.Millisecond)
	begin := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled Run reported success")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("output worker retained cancelled Run")
	}
	if time.Since(begin) > 4*time.Second {
		t.Fatal("unbounded Run cleanup")
	}
	if w.processes != nil || w.commandOutput != nil {
		t.Fatal("Run retained child owner or output callback")
	}
	db, err := taskstore.Open(taskDirectory(paths))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	processes, err := db.Processes(t.Context(), agent.session.ID)
	if err != nil || len(processes) != 1 || processes[0].State != task.ProcessStopped || processes[0].Effects != "unknown" {
		t.Fatalf("cleanup not durable before exit: %+v %v", processes, err)
	}
	lease, err := db.Acquire(t.Context(), taskstore.NewID(), w.root)
	if err != nil {
		t.Fatalf("cancelled Run still owns lease: %v", err)
	}
	_ = lease.Close()
	if writer.Err() == nil || (!errors.Is(writer.Err(), context.Canceled) && !errors.Is(writer.Err(), os.ErrDeadlineExceeded) && !errors.Is(writer.Err(), io.ErrClosedPipe)) {
		t.Fatalf("output cancellation missing: %v", writer.Err())
	}
}

func TestSignalShutdownWithUnreadStdoutRecordsProcessBeforeExit(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launch, _ := json.Marshal(helperInput(executable, "burst-wait", 30))
	var requests atomic.Int32
	active := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		if requests.Add(1) == 1 {
			response := map[string]any{"type": "response.completed", "response": map[string]any{"id": "start-response", "status": "completed", "output": []any{map[string]any{"type": "function_call", "id": "start-item", "call_id": "start-call", "name": "start_process", "arguments": string(launch)}}}}
			encoded, _ := json.Marshal(response)
			out.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(out, "event: response.completed\ndata: "+string(encoded)+"\n\n")
			return
		}
		if requests.Load() == 2 {
			close(active)
		}
		select {
		case <-request.Context().Done():
		case <-t.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	paths := shutdownPaths(t, server.URL)
	root := t.TempDir()
	args, _ := json.Marshal([]string{"--workspace", root, "--prompt", "run command", "--auto-approve", "--allow-executable", executable})
	command := exec.Command(executable, "-test.run=^TestTaskShutdownHelperProcess$")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "OPENAI_") || strings.HasPrefix(key, "MELDRA_") && key != "MELDRA_TEST_HOME" {
			continue
		}
		command.Env = append(command.Env, entry)
	}
	command.Env = append(command.Env, "MELDRA_HOME="+paths.Home, "MELDRA_SHUTDOWN_HELPER=1", "MELDRA_SHUTDOWN_ARGS="+string(args), "TERM=dumb")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	command.Stdout = writer
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = command.Process.Kill()
			<-done
		}
	})
	select {
	case <-active:
	case err := <-done:
		finished = true
		t.Fatalf("child exited before command: %v %s", err, stderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("command did not start")
	}
	time.Sleep(100 * time.Millisecond)
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		finished = true
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM blocked behind unread stdout")
	}
	if status, ok := command.ProcessState.Sys().(syscall.WaitStatus); !ok || status.Signaled() {
		t.Fatalf("not graceful shutdown: %v %s", command.ProcessState, stderr.String())
	}
	db, record := assertStoppedTask(t, paths)
	processes, err := db.Processes(t.Context(), record.ID)
	if err != nil || len(processes) != 1 || processes[0].State != task.ProcessStopped || processes[0].Effects != "unknown" {
		t.Fatalf("missing shutdown record: %+v %v", processes, err)
	}
	lease, err := db.Acquire(t.Context(), taskstore.NewID(), root)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Close()
}

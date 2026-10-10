//go:build darwin || linux

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/charmbracelet/x/term"

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

func TestSynchronizedWriterRecoversFromStallAndKeepsFailure(t *testing.T) {
	writer, reader := unreadOutput(t, t.Context())
	started := time.Now()
	n, err := writer.Write([]byte(strings.Repeat("x", 4<<20)))
	if !errors.Is(err, os.ErrDeadlineExceeded) || n == 4<<20 || time.Since(started) > 3*time.Second {
		t.Fatalf("unbounded output n=%d err=%v elapsed=%s", n, err, time.Since(started))
	}
	const result = "late result\n"
	done := make(chan []byte, 1)
	go func() {
		data := make([]byte, n+len(resumedOutputNotice)+len(result))
		_, _ = io.ReadFull(reader, data)
		done <- data
	}()
	if _, err := writer.Write([]byte(result)); err != nil {
		t.Fatalf("output remained disabled after a temporary stall: %v", err)
	}
	select {
	case got := <-done:
		if want := strings.Repeat("x", n) + resumedOutputNotice + result; string(got) != want {
			t.Fatalf("recovery lost its marker or repeated partial payload: %q", got[max(0, len(got)-200):])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resumed output did not reach its reader")
	}
	if !errors.Is(writer.Err(), os.ErrDeadlineExceeded) {
		t.Fatalf("output failure was hidden: %v", writer.Err())
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

func TestCommandPresentationRecoversAfterTemporaryOutputStall(t *testing.T) {
	writer, reader := unreadOutput(t, t.Context())
	w, _ := testWorkspace(t, t.TempDir(), "", true)
	a := &Agent{workspace: w, output: writer}
	stop := a.startCommandPresentation(t.Context())
	defer stop()
	w.commandOutput(CommandOutput{Text: strings.Repeat("x", 4<<20)})
	deadline := time.Now().Add(4 * time.Second)
	for writer.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !errors.Is(writer.Err(), os.ErrDeadlineExceeded) {
		t.Fatalf("live output did not encounter the test stall: %v", writer.Err())
	}
	const marker = "output after reader resumes\n"
	done := make(chan string, 1)
	go func() {
		var received strings.Builder
		buffer := make([]byte, 32<<10)
		for {
			n, err := reader.Read(buffer)
			received.Write(buffer[:n])
			if strings.Contains(received.String(), marker) || err != nil {
				done <- received.String()
				return
			}
		}
	}()
	w.commandOutput(CommandOutput{Text: marker})
	select {
	case received := <-done:
		if !strings.Contains(received, resumedOutputNotice) || !strings.Contains(received, marker) {
			t.Fatalf("missing recovery notice or resumed live output: %q", received[max(0, len(received)-200):])
		}
	case <-time.After(4 * time.Second):
		t.Fatal("presentation worker stopped permanently after a transient output deadline")
	}
}

func TestOutputOwnerAbruptExitHelper(t *testing.T) {
	mode := os.Getenv("MELDRA_OUTPUT_OWNER_HELPER")
	if mode == "" {
		return
	}
	writer, _, err := newSynchronizedWriter(context.Background(), os.Stdout)
	if err == nil && mode == "relay" {
		_ = writer.Close()
		var bounded *os.File
		var cleanup func() error
		bounded, cleanup, err = relayedOutput(os.Stdout)
		writer = &synchronizedWriter{writer: bounded, terminal: os.Stdout, ctx: context.Background(), deadline: bounded, cleanup: cleanup, live: true}
	}
	if err != nil {
		_, _ = io.WriteString(os.Stderr, err.Error()+"\n")
		os.Exit(2)
	}
	if mode == "tty" && !term.IsTerminal(writer.Fd()) {
		_, _ = io.WriteString(os.Stderr, "terminal identity lost\n")
		os.Exit(3)
	}
	_, _ = io.WriteString(os.Stderr, "output ready\n")
	_, _ = writer.Write([]byte(strings.Repeat("x", 4<<20)))
	for {
		time.Sleep(time.Second)
	}
}

func TestOutputAbruptExitPreservesInheritedDescriptorFlags(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for the pipe and PTY owner-crash regression")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const script = `
import fcntl, os, pty, select, signal, subprocess, sys, time
for mode in ('pipe', 'tty', 'relay'):
    reader, writer = pty.openpty() if mode == 'tty' else os.pipe()
    mask = os.O_NONBLOCK | os.O_APPEND | os.O_ASYNC | os.O_ACCMODE
    original = fcntl.fcntl(writer, fcntl.F_GETFL) & mask
    env = dict(os.environ, MELDRA_OUTPUT_OWNER_HELPER=mode)
    child = subprocess.Popen([sys.argv[1], '-test.run=^TestOutputOwnerAbruptExitHelper$'], stdout=writer, stderr=subprocess.PIPE, env=env)
    try:
        assert select.select([child.stderr], [], [], 8)[0], 'owner did not initialize'
        assert child.stderr.readline() == b'output ready\n', 'owner failed to initialize'
        assert fcntl.fcntl(writer, fcntl.F_GETFL) & mask == original, 'caller flags changed during output initialization'
        time.sleep(.2)
        rows = subprocess.check_output(['ps', '-axo', 'pid,ppid,pgid,state'], text=True).splitlines()[1:]
        groups = {int(row.split()[2]) for row in rows if len(row.split()) >= 4 and int(row.split()[1]) == child.pid}
        if mode == 'relay':
            assert len(groups) == 1, ('expected private relay group', groups)
        child.kill()
        child.wait(timeout=3)
        assert fcntl.fcntl(writer, fcntl.F_GETFL) & mask == original, 'caller flags changed after abrupt owner exit'
        until = time.monotonic() + 4
        while groups:
            rows = subprocess.check_output(['ps', '-axo', 'pid,ppid,pgid,state'], text=True).splitlines()[1:]
            live = [row for row in rows if len(row.split()) >= 4 and int(row.split()[2]) in groups and not row.split()[3].startswith('Z')]
            if not live:
                break
            assert time.monotonic() < until, ('owner crash left live relay processes', live)
            time.sleep(.05)
    finally:
        if child.poll() is None:
            child.kill()
            child.wait(timeout=3)
        child.stderr.close()
        os.close(reader)
        os.close(writer)
print('blocking pipe, PTY, and forced relay preserve flags and stop after owner SIGKILL')
`
	command := exec.CommandContext(t.Context(), python, "-c", script, executable)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("abrupt output-owner exit regression: %v\n%s", err, output)
	}
}

func TestOutputRelayBoundsCleanupWithoutDrainingCallerPipe(t *testing.T) {
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer output.Close()
	// Inherited stdout is normally blocking. Fd makes that test condition
	// explicit before the relay sees the descriptor.
	fd := output.Fd()
	before, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	bounded, cleanup, err := relayedOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	writer := &synchronizedWriter{writer: bounded, terminal: output, ctx: t.Context(), deadline: bounded, cleanup: cleanup, live: true}
	defer writer.Close()
	_, err = writer.Write([]byte(strings.Repeat("x", 4<<20)))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("unread relay did not time out: %v", err)
	}
	started := time.Now()
	if err := writer.Close(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("relay cleanup lost output failure: %v", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("blocked relay prevented bounded cleanup")
	}
	after, _, errno := syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
	const statusFlags = syscall.O_NONBLOCK | syscall.O_APPEND | syscall.O_ASYNC | syscall.O_ACCMODE
	if errno != 0 || before&statusFlags != after&statusFlags {
		t.Fatalf("relay changed caller flags: before=%x after=%x error=%v", before, after, errno)
	}
}

type partiallyStalledOutput struct {
	bytes.Buffer
	writes int
}

func (w *partiallyStalledOutput) SetWriteDeadline(time.Time) error { return nil }
func (w *partiallyStalledOutput) Write(data []byte) (int, error) {
	w.writes++
	if w.writes <= 2 {
		n, _ := w.Buffer.Write(data[:3])
		return n, os.ErrDeadlineExceeded
	}
	return w.Buffer.Write(data)
}

func TestSynchronizedWriterResumesPartialNoticeWithoutReplayingPayload(t *testing.T) {
	output := &partiallyStalledOutput{}
	writer := &synchronizedWriter{writer: output, deadline: output, ctx: t.Context()}
	if n, err := writer.Write([]byte("original")); n != 3 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("initial partial write: %d %v", n, err)
	}
	if n, err := writer.Write([]byte("second")); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("partial notice must not report user bytes written: %d %v", n, err)
	}
	if n, err := writer.Write([]byte("last")); n != 4 || err != nil {
		t.Fatalf("recovery: %d %v", n, err)
	}
	if want := "ori" + resumedOutputNotice + "last"; output.String() != want {
		t.Fatalf("resumed output replayed skipped bytes or repeated notice: got %q want %q", output.String(), want)
	}
	if !errors.Is(writer.Err(), os.ErrDeadlineExceeded) {
		t.Fatal("recovery concealed prior timeout")
	}
}

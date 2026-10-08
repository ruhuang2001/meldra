//go:build darwin || linux

package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
)

// Running Main in a subprocess tests actual signal registration and os.Exit,
// rather than treating context cancellation in an in-process unit test as proof.
func TestTaskShutdownHelperProcess(t *testing.T) {
	if os.Getenv("MELDRA_SHUTDOWN_HELPER") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("MELDRA_SHUTDOWN_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"meldra"}, args...)
	Main("shutdown-test")
	os.Exit(0)
}

type shutdownProcess struct {
	cmd    *exec.Cmd
	output bytes.Buffer
	done   chan struct{}
	err    error
}

func startShutdownProcess(t *testing.T, paths ConfigPaths, args []string, terminal bool) (*shutdownProcess, io.WriteCloser) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestTaskShutdownHelperProcess$")
	if terminal {
		python, err := exec.LookPath("python3")
		if err != nil {
			t.Fatal("terminal integration tests require python3:", err)
		}
		command = exec.Command(python, "-c", shutdownPTYController, executable, "-test.run=^TestTaskShutdownHelperProcess$")
	}
	for _, variable := range os.Environ() {
		key, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(key, "OPENAI_") || strings.HasPrefix(key, "MELDRA_") || key == "TERM" {
			continue
		}
		command.Env = append(command.Env, variable)
	}
	command.Env = append(command.Env, "MELDRA_HOME="+paths.Home, "MELDRA_SHUTDOWN_HELPER=1", "MELDRA_SHUTDOWN_ARGS="+string(encoded), "TERM=xterm-256color")
	process := &shutdownProcess{cmd: command, done: make(chan struct{})}
	command.Stdout = &process.output
	command.Stderr = &process.output
	var input io.WriteCloser
	if terminal {
		input, err = command.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		process.err = command.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		if input != nil {
			_ = input.Close()
		}
		select {
		case <-process.done:
		default:
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case <-process.done:
			case <-time.After(10 * time.Second):
				_ = command.Process.Kill()
				<-process.done
			}
		}
	})
	return process, input
}

func (p *shutdownProcess) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
	case <-time.After(12 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
		t.Fatalf("foreground shutdown exceeded its bound: %s", p.output.String())
	}
	if status, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		t.Fatalf("process died from %s instead of recording cancellation: %s", status.Signal(), p.output.String())
	}
	if p.cmd.ProcessState.ExitCode() > 1 {
		t.Fatalf("shutdown exit = %v: %s", p.err, p.output.String())
	}
}

func shutdownPaths(t *testing.T, baseURL string) ConfigPaths {
	t.Helper()
	paths, err := ConfigPathsForHome(filepath.Join(t.TempDir(), "meldra-home"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveConfig(paths, Config{Model: "local-shutdown-fixture", BaseURL: baseURL, AllowInsecureBaseURL: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveAPIKey(paths, "fixture-key"); err != nil {
		t.Fatal(err)
	}
	return paths
}

func assertStoppedTask(t *testing.T, paths ConfigPaths) (*taskstore.Store, task.Task) {
	t.Helper()
	db, err := taskstore.Open(taskDirectory(paths))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tasks, err := db.ListTasks(t.Context(), "", 10)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("tasks after process exit = %#v, %v", tasks, err)
	}
	record := tasks[0]
	if record.Status != task.Cancelled && record.Status != task.Interrupted {
		t.Fatalf("task after cancellation = %s", record.Status)
	}
	runs, err := db.Runs(t.Context(), record.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs = %#v, %v", runs, err)
	}
	if !runs[0].Status.Terminal() || runs[0].Status == task.RunSucceeded || runs[0].EndedAt.IsZero() {
		t.Fatalf("unfinished or falsely successful run after exit: %#v", runs[0])
	}
	return db, record
}

func TestTaskSignalsCancelStreamingAndPersist(t *testing.T) {
	for _, signal := range []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(signal.String(), func(t *testing.T) {
			active := make(chan struct{})
			closed := make(chan struct{})
			var started, finished sync.Once
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, request.Body)
				writer.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"working\"}\n\n")
				writer.(http.Flusher).Flush()
				started.Do(func() { close(active) })
				select {
				case <-request.Context().Done():
				case <-t.Context().Done():
				}
				finished.Do(func() { close(closed) })
			}))
			t.Cleanup(server.Close)
			paths := shutdownPaths(t, server.URL)
			process, _ := startShutdownProcess(t, paths, []string{"--workspace", t.TempDir(), "--prompt", "wait for cancellation"}, false)
			select {
			case <-active:
			case <-process.done:
				t.Fatalf("process exited before streaming: %s", process.output.String())
			case <-time.After(15 * time.Second):
				t.Fatal("local model stream did not start")
			}
			if err := process.cmd.Process.Signal(signal); err != nil {
				t.Fatal(err)
			}
			process.wait(t)
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("provider request was not cancelled")
			}
			_, record := assertStoppedTask(t, paths)
			if record.Status != task.Cancelled {
				t.Fatalf("inference-only cancellation should be confirmed: %#v", record)
			}
			if requests.Load() != 1 {
				t.Fatalf("new work was scheduled during shutdown: %d requests", requests.Load())
			}
		})
	}
}

func TestTaskSignalStopsOwnedCommandGroup(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("command integration tests require python3:", err)
	}
	workspace := t.TempDir()
	script := `import json, os, pathlib, subprocess, sys, time
child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(90)"])
pathlib.Path("owned-pids.json").write_text(json.dumps([os.getpid(), child.pid]))
time.sleep(90)
`
	if err := os.WriteFile(filepath.Join(workspace, "owned.py"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_owned","status":"completed","output":[{"type":"function_call","id":"fc_owned","call_id":"call_owned","status":"completed","name":"run_command","arguments":"{\"command\":\"python3\",\"args\":[\"owned.py\"],\"timeout\":120}"}]}}`+"\n\n")
	}))
	t.Cleanup(server.Close)
	paths := shutdownPaths(t, server.URL)
	process, _ := startShutdownProcess(t, paths, []string{"--workspace", workspace, "--auto-approve", "--prompt", "run the owned command"}, false)
	var pids []int
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	for len(pids) != 2 {
		data, _ := os.ReadFile(filepath.Join(workspace, "owned-pids.json"))
		_ = json.Unmarshal(data, &pids)
		if len(pids) == 2 {
			break
		}
		select {
		case <-process.done:
			t.Fatalf("command failed before starting children: %s", process.output.String())
		case <-deadline.C:
			t.Fatal("owned command did not become ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// These PIDs belong to this fixture; cleanup is bounded even after a failed assertion.
	t.Cleanup(func() {
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	if err := process.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	process.wait(t)
	for _, pid := range pids {
		assertProcessStopped(t, pid)
	}
	db, record := assertStoppedTask(t, paths)
	calls, err := db.ToolCalls(t.Context(), record.ID)
	if err != nil || len(calls) != 1 {
		t.Fatalf("tool calls after signal = %#v, %v", calls, err)
	}
	if calls[0].Status != task.ToolCancelled && calls[0].Status != task.ToolUnknown {
		t.Fatalf("interrupted command status = %s", calls[0].Status)
	}
	if requests.Load() != 1 {
		t.Fatalf("new model turn after command cancellation: %d", requests.Load())
	}
}

func assertProcessStopped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		// A reparented zombie cannot execute. Linux CI's init may reap it later.
		status, _ := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		if state := strings.TrimSpace(string(status)); state == "" || strings.HasPrefix(state, "Z") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("owned process %d still runs after task exit (state %s)", pid, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTaskTerminalCloseCancelsForeground(t *testing.T) {
	active := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"working\"}\n\n")
		writer.(http.Flusher).Flush()
		once.Do(func() { close(active) })
		select {
		case <-request.Context().Done():
		case <-t.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	paths := shutdownPaths(t, server.URL)
	process, terminalInput := startShutdownProcess(t, paths, []string{"--workspace", t.TempDir(), "--prompt", "wait for terminal close"}, true)
	select {
	case <-active:
	case <-process.done:
		t.Fatalf("terminal process exited before inference: %s", process.output.String())
	case <-time.After(20 * time.Second):
		t.Fatal("terminal task did not start")
	}
	if _, err := io.WriteString(terminalInput, "close\n"); err != nil {
		t.Fatal(err)
	}
	process.wait(t)
	var result struct {
		Exit   int    `json:"exit"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(process.output.Bytes(), &result); err != nil {
		t.Fatalf("terminal controller result: %v: %s", err, process.output.String())
	}
	if result.Exit < 0 || result.Exit > 1 {
		t.Fatalf("terminal child did not exit gracefully: %#v", result)
	}
	assertStoppedTask(t, paths)
}

func TestTaskPromptEOFCompletesWithoutFalseHangup(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_eof","status":"completed","output":[{"type":"message","id":"msg_eof","status":"completed","role":"assistant","content":[{"type":"output_text","text":"done","annotations":[]}]}]}}`+"\n\n")
	}))
	t.Cleanup(server.Close)
	paths := shutdownPaths(t, server.URL)
	// The subprocess gets /dev/null for stdin. EOF after --prompt is normal input
	// completion, even though closing an actual controlling terminal cancels work.
	process, _ := startShutdownProcess(t, paths, []string{"--workspace", t.TempDir(), "--prompt", "complete this prompt"}, false)
	process.wait(t)
	if process.err != nil {
		t.Fatalf("prompt with EOF failed: %v: %s", process.err, process.output.String())
	}
	db, err := taskstore.Open(taskDirectory(paths))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.ListTasks(t.Context(), "", 10)
	if err != nil || len(tasks) != 1 || tasks[0].Status != task.Completed {
		t.Fatalf("prompt EOF task = %#v, %v", tasks, err)
	}
	runs, err := db.Runs(t.Context(), tasks[0].ID)
	if err != nil || len(runs) != 1 || runs[0].Status != task.RunSucceeded {
		t.Fatalf("prompt EOF runs = %#v, %v", runs, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("prompt EOF requests = %d", requests.Load())
	}
}

// Python's standard library provides a real controlling terminal on both target
// operating systems. Closing the master delivers the OS terminal-hangup event;
// the controller never sends SIGHUP itself and never uses a paid model endpoint.
const shutdownPTYController = `
import fcntl, json, os, pty, select, signal, struct, subprocess, sys, termios
master, slave = pty.openpty()
fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 24, 80, 0, 0))
def session():
    os.setsid()
    fcntl.ioctl(slave, termios.TIOCSCTTY, 0)
child = subprocess.Popen(sys.argv[1:], stdin=slave, stdout=slave, stderr=slave, preexec_fn=session)
os.close(slave)
output = bytearray()
def stop(signum, frame):
    raise SystemExit(1)
signal.signal(signal.SIGTERM, stop)
try:
    while child.poll() is None:
        ready, _, _ = select.select([master, 0], [], [], 0.1)
        if 0 in ready:
            os.read(0, 1024)
            break
        if master in ready:
            try:
                data = os.read(master, 65536)
                if not data:
                    break
                output.extend(data)
            except OSError:
                break
    os.close(master)
    code = child.wait(timeout=8)
    print(json.dumps({"exit": code, "output": output.decode(errors="replace")}))
except BaseException:
    child.kill()
    child.wait()
    raise
`

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"sync"
	"time"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
	"meldra/internal/tool"
)

// processManager is owned by one Run, not by a tool invocation. Workers retain
// immutable store identities and their own contexts, never Workspace.ctx, a
// taskExecution.current pointer, or a synchronous tool Observation.
type processManager struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	closed bool
	epoch  string
	taskID string
	runID  string
	db     *taskstore.Store
	lease  *taskstore.Lease
	// Bound once before launch; separates lifecycle persistence from process
	// ownership and permits deterministic persistence-fault validation.
	persistProcess func(context.Context, *taskstore.Lease, task.Process) error
	processes      map[string]*managedProcess
	err            error
}

type managedProcess struct {
	record task.Process // protected by manager.mu
	log    *commandLog
	cancel context.CancelFunc
	done   chan struct{}
}

func (w *Workspace) beginProcesses(ctx context.Context, execution *taskExecution) error {
	if w.processes != nil {
		return fmt.Errorf("process owner already active")
	}
	runCtx, cancel := context.WithCancel(ctx)
	m := &processManager{ctx: runCtx, cancel: cancel, epoch: taskstore.NewID(), processes: make(map[string]*managedProcess)}
	if execution != nil {
		m.taskID, m.runID, m.db, m.lease = execution.session.ID, execution.run.ID, execution.db, execution.lease
		m.persistProcess = execution.db.SaveProcess
	}
	w.processes = m
	return nil
}

func (w *Workspace) closeProcesses() error {
	if w.processes == nil {
		return nil
	}
	m := w.processes
	m.mu.Lock()
	m.closed = true
	m.cancel()
	processes := make([]*managedProcess, 0, len(m.processes))
	for _, p := range m.processes {
		processes = append(processes, p)
	}
	m.mu.Unlock()
	for _, p := range processes {
		<-p.done
	}
	m.mu.Lock()
	err := m.err
	for _, p := range m.processes {
		if p.record.Effects == "unknown" {
			err = errors.Join(err, fmt.Errorf("%w: process %s requires reconciliation", task.ErrUnresolved, p.record.ID))
		}
	}
	m.mu.Unlock()
	w.processes = nil
	return err
}

func (w *Workspace) processWriteConflict(name string, raw json.RawMessage) error {
	if w.processes == nil {
		return nil
	}
	switch name {
	case "edit_file", "apply_patch", "undo_last_change", "run_command", "start_process", "verify":
	default:
		return nil
	}
	if name == "verify" {
		var in struct {
			Preset string `json:"preset"`
		}
		if json.Unmarshal(raw, &in) == nil && in.Preset == "diff" {
			return nil
		}
	}
	m := w.processes
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	for _, p := range m.processes {
		if p.record.Effects == "unknown" {
			return fmt.Errorf("%w: process %s has unknown effects", task.ErrUnresolved, p.record.ID)
		}
		if !p.record.State.Terminal() {
			return fmt.Errorf("%w: process %s may write the workspace; stop or await it before editing or launching another command", ErrWorkspaceBusy, p.record.ID)
		}
	}
	return nil
}

// ProcessToolDefinitions are deliberately separate from ToolDefinitions:
// stateless MCP server requests cannot own handles beyond their request Run.
func (w *Workspace) ProcessToolDefinitions() []ToolDefinition {
	str := func(description string) map[string]any {
		return map[string]any{"type": "string", "description": description}
	}
	integer := func(description string) map[string]any {
		return map[string]any{"type": []string{"integer", "null"}, "description": description}
	}
	status := map[string]any{"process_id": str("Managed process ID from this Run."), "cursor": integer("Raw byte cursor from the preceding output; null starts at zero.")}
	wait := map[string]any{"process_id": status["process_id"], "cursor": status["cursor"], "wait_ms": integer("Maximum observation wait in milliseconds, 0..10000; null defaults to 1000. This does not change the command deadline.")}
	return w.guardedDefinitions([]ToolDefinition{
		{Name: "start_process", Description: "Start one approved command owned by this Run. Returns a handle, not command success. Stop or await it before file edits or other commands. It is stopped when this Run ends; no background daemon or cross-turn lifetime.", Parameters: objectSchema(map[string]any{"command": str("Allowlisted executable name or explicitly configured absolute path."), "args": map[string]any{"type": []string{"array", "null"}, "items": str("One argument; no shell expansion.")}, "timeout": integer("Total execution deadline seconds; null uses configured default, maximum 86400.")}, []string{"command", "args", "timeout"}), Function: w.bindTool(w.startProcess)},
		{Name: "process_status", Description: "Read managed process state and bounded incremental output without extending its lifetime.", Parameters: objectSchema(status, []string{"process_id", "cursor"}), Function: w.bindTool(w.processStatus)},
		{Name: "wait_process", Description: "Wait a bounded duration for a managed process. Cancelling this observation does not stop the process; Run cancellation does.", Parameters: objectSchema(wait, []string{"process_id", "cursor", "wait_ms"}), Function: w.bindTool(w.waitProcess)},
		{Name: "stop_process", Description: "Stop and reap an owned managed process group. Termination is known; external effects may require reconciliation. Repeating stop is safe.", Parameters: objectSchema(status, []string{"process_id", "cursor"}), Function: w.bindTool(w.stopProcess)},
	})
}

func (w *Workspace) startProcess(raw json.RawMessage) (string, error) {
	var in struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Timeout int      `json:"timeout"`
	}
	if err := decodeToolInput(raw, &in, "command"); err != nil {
		return "", err
	}
	if w.processes == nil {
		return "", fmt.Errorf("managed processes require an active agent Run")
	}
	if err := w.processWriteConflict("start_process", raw); err != nil {
		return "", err
	}
	spec, err := w.prepareCommand(in.Command, in.Args, in.Timeout)
	if err != nil {
		return "", err
	}
	if spec.ApprovalRequired && !w.approveCommandSpec(spec) {
		return "Declined; process not started.", nil
	}
	if err := w.admitOperation(w.ctx); err != nil {
		return "", err
	}
	m := w.processes
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("process Run is stopping")
	}
	if len(m.processes) >= 64 {
		m.mu.Unlock()
		return "", fmt.Errorf("Run process limit reached (64)")
	}
	m.mu.Unlock()
	environment, cleanup, err := newCommandEnvironment()
	if err != nil {
		return "", err
	}
	processID := taskstore.NewID()
	log, err := newCommandLog(w.commandOutput, processID)
	if err != nil {
		cleanup()
		return "", err
	}
	launch, _ := json.Marshal(spec)
	record := task.Process{ID: processID, TaskID: m.taskID, RunID: m.runID, OwnerEpoch: m.epoch, LaunchHash: digest(launch), Executable: spec.Executable, Arguments: slices.Clone(spec.Arguments), Directory: spec.Directory, TimeoutSeconds: spec.TimeoutSeconds, State: task.ProcessStarting, Effects: "pending", StartedAt: time.Now().UTC()}
	if execution, ok := w.ctx.Value(executionContextKey{}).(*taskExecution); ok && execution.current != nil {
		record.ToolCallID = execution.current.ID
	}
	if m.db != nil {
		record, err = m.db.CreateProcess(w.ctx, m.lease, record)
		if err != nil {
			log.close()
			cleanup()
			return "", &persistenceError{err}
		}
	}
	processCtx, cancel := context.WithTimeout(m.ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	p := &managedProcess{record: record, log: log, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.processes[processID] = p
	m.mu.Unlock()
	cmd := exec.CommandContext(processCtx, spec.Executable, spec.Arguments...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = spec.Directory, environment, log, log
	prepareCommandProcess(cmd)
	if err = w.admitOperation(w.ctx); err == nil {
		err = cmd.Start()
	}
	if err != nil {
		m.finishProcess(p, cmd, processCtx, err, false)
		cancel()
		log.close()
		cleanup()
		close(p.done)
		return "", err
	}
	tool.Observe(w.ctx, func(o *tool.Observation) { o.Started = true })
	m.mu.Lock()
	p.record.State, p.record.PID = task.ProcessRunning, cmd.Process.Pid
	running := p.record
	m.mu.Unlock()
	if err = m.save(running); err != nil {
		m.mu.Lock()
		m.err = errors.Join(m.err, err)
		m.mu.Unlock()
		cancel()
	}
	go func() {
		waitErr := waitCommandProcess(processCtx, cmd)
		m.finishProcess(p, cmd, processCtx, waitErr, true)
		cancel()
		log.close()
		cleanup()
		close(p.done)
	}()
	if err != nil {
		<-p.done
		return "", err
	}
	return processSnapshot(m, p, 0)
}

func (m *processManager) save(record task.Process) error {
	if m.db == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.persistProcess(ctx, m.lease, record); err != nil {
		return &persistenceError{err}
	}
	return nil
}

func (m *processManager) finishProcess(p *managedProcess, cmd *exec.Cmd, ctx context.Context, waitErr error, started bool) {
	p.log.finish()
	m.mu.Lock()
	record := p.record
	m.mu.Unlock()
	record.EndedAt, record.Effects = time.Now().UTC(), "known"
	record.State = task.ProcessExited
	if cmd.ProcessState != nil {
		record.ExitCode = new(cmd.ProcessState.ExitCode())
		record.Signal = processSignal(cmd)
	}
	if !started {
		record.State = task.ProcessFailed
	} else if ctx.Err() != nil {
		record.State, record.Effects = task.ProcessStopped, "unknown"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			record.State = task.ProcessTimedOut
		}
	} else if _, exited := errors.AsType[*exec.ExitError](waitErr); waitErr != nil && !exited {
		record.State, record.Effects = task.ProcessUnknown, "unknown"
	}
	if waitErr != nil {
		record.Error = truncateSessionMessage(waitErr.Error())
	}
	data, truncated, logErr := p.log.artifact()
	p.log.mu.Lock()
	record.OutputEnd = p.log.total
	record.OutputStart = max(int64(0), p.log.total-int64(len(p.log.tail)))
	p.log.mu.Unlock()
	record.Truncated = truncated || logErr != nil
	if m.db != nil && len(data) > 0 {
		saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		artifact, err := m.db.PutArtifact(saveCtx, m.lease, m.taskID, "process-"+record.ID+".log", data)
		cancel()
		if err == nil {
			record.Artifacts = []task.ArtifactRef{artifact}
		} else {
			record.Truncated = true
			if !errors.Is(err, task.ErrArtifactLimit) {
				m.mu.Lock()
				m.err = errors.Join(m.err, &persistenceError{err})
				m.mu.Unlock()
			}
		}
	}
	err := m.save(record)
	m.mu.Lock()
	p.record = record
	if err != nil {
		m.err = errors.Join(m.err, err)
	}
	m.mu.Unlock()
}

type processInput struct {
	ProcessID string `json:"process_id"`
	Cursor    int64  `json:"cursor"`
	WaitMS    *int   `json:"wait_ms"`
}

func (w *Workspace) lookupProcess(raw json.RawMessage, wait bool) (*processManager, *managedProcess, processInput, error) {
	var in processInput
	if err := decodeToolInput(raw, &in, "process_id"); err != nil {
		return nil, nil, in, err
	}
	if in.Cursor < 0 || (!wait && in.WaitMS != nil) {
		return nil, nil, in, fmt.Errorf("invalid process observation arguments")
	}
	if w.processes == nil {
		return nil, nil, in, fmt.Errorf("process handles are valid only in their owning Run")
	}
	m := w.processes
	m.mu.Lock()
	p := m.processes[in.ProcessID]
	m.mu.Unlock()
	if p == nil {
		return nil, nil, in, fmt.Errorf("process %q does not belong to this Run", in.ProcessID)
	}
	return m, p, in, nil
}

func processSnapshot(m *processManager, p *managedProcess, cursor int64) (string, error) {
	output, next, truncated, err := p.log.read(cursor)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	record := p.record
	storeErr := m.err
	m.mu.Unlock()
	if storeErr != nil {
		return "", storeErr
	}
	data, err := json.Marshal(struct {
		Process   task.Process `json:"process"`
		Output    string       `json:"output"`
		Cursor    int64        `json:"cursor"`
		Truncated bool         `json:"truncated,omitzero"`
	}{record, output, next, truncated})
	return string(data), err
}

func (w *Workspace) processStatus(raw json.RawMessage) (string, error) {
	m, p, in, err := w.lookupProcess(raw, false)
	if err != nil {
		return "", err
	}
	return processSnapshot(m, p, in.Cursor)
}

func (w *Workspace) waitProcess(raw json.RawMessage) (string, error) {
	m, p, in, err := w.lookupProcess(raw, true)
	if err != nil {
		return "", err
	}
	wait := 1000
	if in.WaitMS != nil {
		wait = *in.WaitMS
	}
	if wait < 0 || wait > 10000 {
		return "", fmt.Errorf("wait_ms must be 0..10000")
	}
	timer := time.NewTimer(time.Duration(wait) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
	case <-w.ctx.Done():
		return "", w.ctx.Err()
	}
	return processSnapshot(m, p, in.Cursor)
}

func (w *Workspace) stopProcess(raw json.RawMessage) (string, error) {
	m, p, in, err := w.lookupProcess(raw, false)
	if err != nil {
		return "", err
	}
	if err := w.admitOperation(w.ctx); err != nil {
		return "", err
	}
	p.cancel()
	<-p.done
	return processSnapshot(m, p, in.Cursor)
}

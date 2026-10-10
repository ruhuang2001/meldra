package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
)

type controlContextKey struct{}

func controlFromContext(ctx context.Context) *turnControl {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(controlContextKey{}).(*turnControl)
	return c
}

type ControlRequest struct {
	ID         string              `json:"id"`
	Kind       string              `json:"kind"`
	Text       string              `json:"text,omitempty"`
	References []ReferenceSnapshot `json:"references,omitempty"`
}

var errTurnStopped = errors.New("turn stopped by user")

// turnControl owns concurrent input. The Agent remains the sole conversation
// writer; control callbacks only journal requests and cancel bounded operations.
type turnControl struct {
	mu              sync.Mutex
	active          bool
	policy          *RuntimePolicy
	db              *taskstore.Store
	lease           *taskstore.Lease
	taskID, runID   string
	cancel          context.CancelCauseFunc
	inferenceCancel context.CancelFunc
	approvalCancel  context.CancelFunc
	pending         []ControlRequest
	selected        *ControlRequest
	recovered       []ControlRequest
}

func (a *Agent) initControl() *turnControl {
	a.controlInit.Do(func() { a.control = &turnControl{} })
	return a.control
}

func (c *turnControl) record(kind string, request ControlRequest) error {
	if c.db == nil {
		return nil
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.db.AppendEvent(ctx, c.lease, task.Event{TaskID: c.taskID, RunID: c.runID, Kind: kind, Data: raw})
}

// SubmitControl accepts a human control request only while the turn owns its
// durable journal. Success means received, not yet applied or stopped.
func (a *Agent) SubmitControl(kind, text string) error {
	if kind != "steer" && kind != "queue" && kind != "stop" && kind != "mode" {
		return fmt.Errorf("unknown control %q", kind)
	}
	if (kind == "steer" || kind == "queue") && (strings.TrimSpace(text) == "" || len(text) > maxSessionMessageBytes) {
		return fmt.Errorf("control text must be nonempty and at most %d bytes", maxSessionMessageBytes)
	}
	if kind == "mode" && text != "plan" && text != "build" {
		return fmt.Errorf("mode must be plan or build")
	}
	c := a.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.active {
		return ErrAgentBusy
	}
	if len(c.pending) >= 32 {
		return fmt.Errorf("control queue is full")
	}
	r := ControlRequest{ID: taskstore.NewID(), Kind: kind, Text: text}
	if kind == "steer" || kind == "queue" {
		_, refs, err := a.prepareControlReferences(context.Background(), r)
		if err != nil {
			return err
		}
		r.References = refs
	}
	if err := c.record("control.received", r); err != nil {
		c.policy.invalidate()
		c.cancel(&persistenceError{err})
		return err
	}
	c.pending = append(c.pending, r)
	if kind != "queue" {
		c.policy.invalidate()
		if c.inferenceCancel != nil {
			c.inferenceCancel()
		}
		if c.approvalCancel != nil {
			c.approvalCancel()
		}
	}
	if kind == "stop" || kind == "mode" {
		c.cancel(errTurnStopped)
	}
	return nil
}

func (a *Agent) beginControl(ctx context.Context, cancel context.CancelCauseFunc) error {
	c := a.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = true
	c.policy = a.policy
	c.cancel = cancel
	if e := a.execution; e != nil {
		c.db = e.db
		c.lease = e.lease
		c.taskID = e.session.ID
		c.runID = e.run.ID
	}
	return nil
}

func (a *Agent) endControl() {
	c := a.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = false
	c.cancel = nil
	c.inferenceCancel = nil
	c.approvalCancel = nil
	c.db = nil
	c.lease = nil
}

func (a *Agent) inferenceContext(ctx context.Context) (context.Context, func()) {
	child, cancel := context.WithCancel(ctx)
	c := a.initControl()
	c.mu.Lock()
	c.inferenceCancel = cancel
	for _, r := range c.pending {
		if r.Kind == "steer" {
			cancel()
			break
		}
	}
	c.mu.Unlock()
	return child, func() { c.mu.Lock(); c.inferenceCancel = nil; c.mu.Unlock(); cancel() }
}

func (c *turnControl) approvalContext(ctx context.Context) (context.Context, func()) {
	child, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.approvalCancel = cancel
	c.mu.Unlock()
	return child, func() { c.mu.Lock(); c.approvalCancel = nil; c.mu.Unlock(); cancel() }
}

func (a *Agent) hasSteering() bool {
	c := a.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.pending {
		if r.Kind == "steer" {
			return true
		}
	}
	return false
}

func (a *Agent) applySteering(ctx context.Context) (string, error) {
	c := a.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	var text strings.Builder
	remaining := make([]ControlRequest, 0, len(c.pending))
	for _, r := range c.pending {
		if r.Kind != "steer" {
			remaining = append(remaining, r)
			continue
		}
		augmented, refs, err := a.prepareControlReferences(ctx, r)
		if err != nil {
			return "", err
		}
		if a.session != nil {
			a.session.appendMessage("user", r.Text)
			a.session.Messages[len(a.session.Messages)-1].References = refs
			a.session.Messages[len(a.session.Messages)-1].ControlID = r.ID
			a.session.PreviousResponseID = ""
			a.session.Mode, a.session.Permissions, a.session.PolicyGeneration = a.policy.snapshot()
			if err := a.store.Save(a.session); err != nil {
				return "", err
			}
		}
		if err := c.record("control.applied", r); err != nil {
			return "", err
		}
		fmt.Fprintf(&text, "\nUser correction:\n%s\n", augmented)
		a.emit(UIEvent{Kind: UIEventUserMessage, Text: r.Text})
	}
	c.pending = remaining
	return text.String(), nil
}

func parseControlText(text string) (kind, value string, ok bool) {
	command, rest, _ := strings.Cut(strings.TrimSpace(text), " ")
	switch command {
	case "/stop":
		return "stop", "", true
	case "/queue":
		return "queue", strings.TrimSpace(rest), true
	case "/steer":
		return "steer", strings.TrimSpace(rest), true
	case "/plan":
		return "mode", "plan", true
	case "/build":
		return "mode", "build", true
	}
	return "", "", false
}

func (a *Agent) pendingControl() (*ControlRequest, error) {
	c := a.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return nil, nil
	}
	index := 0
	for i, r := range c.pending {
		if r.Kind == "stop" || r.Kind == "mode" {
			index = i
			break
		}
	}
	r := c.pending[index]
	c.pending = append(c.pending[:index], c.pending[index+1:]...)
	c.selected = &r
	return &r, nil
}

func (a *Agent) applyMode(mode ExecutionMode) error {
	if a.modeError != nil {
		return a.modeError
	}
	previousMode, _, _ := a.policy.snapshot()
	failedSave := func(err error) error {
		// Keep the running policy no more permissive than before the failed
		// transition. Advancing again also invalidates every old permit.
		_ = a.policy.setMode(previousMode)
		a.modeError = &persistenceError{fmt.Errorf("record mode change; reload the session before continuing: %w", err)}
		return a.modeError
	}
	if mode == ModeBuild && a.connectExternal != nil {
		if err := a.connectExternal(); err != nil {
			return err
		}
	}
	if a.policy == nil {
		p, err := newRuntimePolicy(mode, PermissionInteractive)
		if err != nil {
			return err
		}
		a.policy = p
	} else if err := a.policy.setMode(mode); err != nil {
		return err
	}
	if a.session != nil {
		next := *a.session
		next.Mode, next.Permissions, next.PolicyGeneration = a.policy.snapshot()
		if mode == ModeBuild && len(next.Plan) > 0 {
			raw, _ := json.Marshal(next.Plan)
			next.ApprovedPlanDigest = digest(raw)
		}
		// A legacy source must be imported before any task snapshot changes its revision.
		if a.session.taskSnapshot || a.execution == nil {
			if a.store == nil {
				return failedSave(fmt.Errorf("session requires a persistence store"))
			}
			if err := a.store.Save(&next); err != nil {
				return failedSave(err)
			}
		}
		*a.session = next
	}
	_, permissions, _ := a.policy.snapshot()
	a.emit(UIEvent{Kind: UIEventMode, Text: string(mode), Detail: string(permissions)})
	a.emit(UIEvent{Kind: UIEventStatus, Text: "Mode: " + string(mode)})
	a.emitNotice("Mode: " + string(mode))
	return nil
}

func (a *Agent) emitNotice(text string) {
	if a.events != nil {
		a.emit(UIEvent{Kind: UIEventNotice, Text: text})
	} else {
		fmt.Fprintln(a.writer(), text)
	}
}

// finishSelected runs after its conversation snapshot is durable, before inference.
func (a *Agent) finishSelected() error {
	c := a.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.selected == nil {
		return nil
	}
	if err := c.record("control.applied", *c.selected); err != nil {
		return err
	}
	c.selected = nil
	return nil
}

func (a *Agent) ackIdleControl(r ControlRequest) error {
	if a.execution == nil {
		return nil
	}
	db, err := taskstore.Open(taskDirectory(a.execution.paths))
	if err != nil {
		return err
	}
	defer db.Close()
	lease, err := db.Acquire(context.Background(), a.session.ID, a.session.Workspace)
	if err != nil {
		return err
	}
	defer lease.Close()
	raw, _ := json.Marshal(r)
	return db.AppendEvent(context.Background(), lease, task.Event{TaskID: a.session.ID, Kind: "control.applied", Data: raw})
}

func (a *Agent) loadPendingControls() error {
	if a.execution == nil {
		return nil
	}
	db, err := taskStoreForRead(a.execution.paths)
	if err != nil {
		return err
	}
	defer db.Close()
	outstanding := map[string]ControlRequest{}
	order := []string{}
	var after int64
	for {
		events, err := db.Events(context.Background(), a.session.ID, after, 1000)
		if err != nil {
			return err
		}
		if len(events) == 0 {
			break
		}
		for _, event := range events {
			after = event.Sequence
			if event.Kind == "turn.started" {
				var started struct {
					ControlID string `json:"control_id"`
				}
				if err := json.Unmarshal(event.Data, &started); err != nil {
					return err
				}
				delete(outstanding, started.ControlID)
				continue
			}
			if event.Kind != "control.received" && event.Kind != "control.applied" && event.Kind != "control.cancelled" {
				continue
			}
			var r ControlRequest
			if err := json.Unmarshal(event.Data, &r); err != nil {
				return err
			}
			if r.ID == "" {
				return fmt.Errorf("invalid control journal identity")
			}
			if event.Kind == "control.received" {
				outstanding[r.ID] = r
				order = append(order, r.ID)
			} else {
				delete(outstanding, r.ID)
			}
		}
	}
	// Snapshot commit may precede the applied event. Identity closes that gap.
	for _, m := range a.session.Messages {
		delete(outstanding, m.ControlID)
	}
	c := a.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range order {
		if r, ok := outstanding[id]; ok {
			c.recovered = append(c.recovered, r)
			delete(outstanding, id)
		}
	}
	if len(c.recovered) > 32 {
		return fmt.Errorf("recovered control queue exceeds 32 requests")
	}
	return nil
}

func (a *Agent) continueRecovered() bool {
	c := a.initControl()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.recovered) == 0 {
		return false
	}
	c.pending = append(c.pending, c.recovered...)
	c.recovered = nil
	return true
}

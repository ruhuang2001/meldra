package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

type ExecutionMode string
type PermissionProfile string

const (
	ModeBuild               ExecutionMode     = "build"
	ModePlan                ExecutionMode     = "plan"
	PermissionInteractive   PermissionProfile = "interactive"
	PermissionWorkspaceEdit PermissionProfile = "workspace-edit"
)

var ErrOperationSuperseded = errors.New("operation superseded by user control; request a new operation")

// RuntimePolicy serializes control changes with admission of new side effects.
// It does not claim to interrupt or reverse an already admitted operation.
type RuntimePolicy struct {
	mu          sync.Mutex
	mode        ExecutionMode
	permissions PermissionProfile
	generation  uint64
}

func newRuntimePolicy(mode ExecutionMode, permissions PermissionProfile) (*RuntimePolicy, error) {
	if mode == "" {
		mode = ModeBuild
	}
	if permissions == "" {
		permissions = PermissionInteractive
	}
	if mode != ModeBuild && mode != ModePlan {
		return nil, fmt.Errorf("mode must be plan or build")
	}
	if permissions != PermissionInteractive && permissions != PermissionWorkspaceEdit {
		return nil, fmt.Errorf("permissions must be interactive or workspace-edit")
	}
	return &RuntimePolicy{mode: mode, permissions: permissions}, nil
}

func (p *RuntimePolicy) snapshot() (ExecutionMode, PermissionProfile, uint64) {
	if p == nil {
		return ModeBuild, PermissionInteractive, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.mode, p.permissions, p.generation
}

func (p *RuntimePolicy) invalidate() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.generation++
}

func (p *RuntimePolicy) setMode(mode ExecutionMode) error {
	if mode != ModeBuild && mode != ModePlan {
		return fmt.Errorf("mode must be plan or build")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mode = mode
	p.generation++
	return nil
}

type operationClass string

const (
	operationRead     operationClass = "read"
	operationMetadata operationClass = "metadata"
	operationWrite    operationClass = "write"
	operationExecute  operationClass = "execute"
	operationControl  operationClass = "process-control"
)

func classifyOperation(name string, input json.RawMessage) operationClass {
	switch name {
	case "read_project_context", "read_file", "read_skill", "list_files", "search_files", "session_status", "git_review", "process_status", "wait_process":
		return operationRead
	case "update_plan", "save_summary":
		return operationMetadata
	case "edit_file", "apply_patch", "undo_last_change":
		return operationWrite
	case "stop_process":
		return operationControl
	case "verify":
		var args struct {
			Preset string `json:"preset"`
		}
		if json.Unmarshal(input, &args) == nil && args.Preset == "diff" {
			return operationRead
		}
	case "run_command":
		var args struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		}
		if json.Unmarshal(input, &args) == nil && args.Command == "git" && allowedGit(args.Args) {
			return operationRead
		}
	}
	return operationExecute
}

type operationPermit struct {
	policy     *RuntimePolicy
	generation uint64
	class      operationClass
}
type operationPermitKey struct{}
type operationGenerationKey struct{}

func (p *RuntimePolicy) invocation(ctx context.Context, name string, input json.RawMessage) (context.Context, error) {
	_, _, generation := p.snapshot()
	if expected, ok := ctx.Value(operationGenerationKey{}).(uint64); ok {
		generation = expected
	}
	permit := &operationPermit{policy: p, generation: generation, class: classifyOperation(name, input)}
	ctx = context.WithValue(ctx, operationPermitKey{}, permit)
	return ctx, admitOperation(ctx)
}

func admitOperation(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	permit, _ := ctx.Value(operationPermitKey{}).(*operationPermit)
	if permit == nil || permit.policy == nil {
		return nil
	}
	p := permit.policy
	p.mu.Lock()
	defer p.mu.Unlock()
	if permit.generation != p.generation {
		return ErrOperationSuperseded
	}
	if p.mode == ModePlan && permit.class != operationRead && permit.class != operationMetadata && permit.class != operationControl {
		return fmt.Errorf("operation is not permitted in Plan mode; switch to Build explicitly")
	}
	return nil
}

func (w *Workspace) admitOperation(ctx context.Context) error { return admitOperation(ctx) }

// SetMode is also available to headless embedders; models cannot call it.
func (a *Agent) SetMode(mode ExecutionMode) error {
	if !a.turnMu.TryLock() {
		return ErrAgentBusy
	}
	defer a.turnMu.Unlock()
	return a.applyMode(mode)
}

func (w *Workspace) guardedDefinitions(definitions []ToolDefinition) []ToolDefinition {
	for i := range definitions {
		definition := definitions[i]
		definitions[i].Function = func(ctx context.Context, input json.RawMessage) (string, error) {
			// An Agent supplies a permit; MCP/direct registry callers need one too.
			if _, ok := ctx.Value(operationPermitKey{}).(*operationPermit); !ok {
				var err error
				ctx, err = w.policy.invocation(ctx, definition.Name, input)
				if err != nil {
					return "", err
				}
			}
			if err := admitOperation(ctx); err != nil {
				return "", err
			}
			class := classifyOperation(definition.Name, input)
			if class == operationExecute || class == operationWrite {
				if err := w.processWriteConflict("run_command", nil); err != nil {
					return "", err
				}
			}
			return definition.Function(ctx, input)
		}
	}
	return definitions
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
	"meldra/internal/tool"
)

type mcpServerCall struct {
	exec        *taskExecution
	call        task.ToolCall
	pre         hookOutcome
	review      ApprovalRequest
	fingerprint string
	expires     time.Time
	declined    bool
}

func planMCPServerCall(ctx context.Context, paths ConfigPaths, w *Workspace, name string, raw json.RawMessage) (*mcpServerCall, error) {
	store := newTaskSessionStore(paths)
	session, err := store.New(w.root)
	if err != nil {
		return nil, err
	}
	e := &taskExecution{paths: paths, session: session, workspace: w, config: task.Config{Provider: "mcp", Workspace: w.root}, hooks: w.hooks, hookSurface: "mcp", hookNotify: w.hookNotify}
	goal := "MCP server: " + name
	if err := e.begin(ctx, goal); err != nil {
		return nil, err
	}
	session.appendMessage("user", goal)
	session.LastRequestSequence = e.requestSequence
	if err := store.Save(session); err != nil {
		return nil, errors.Join(err, e.finish(ctx, err))
	}
	call, err := e.planCall(ctx, "", name, raw)
	if err != nil {
		return nil, errors.Join(err, e.finish(ctx, err))
	}
	return &mcpServerCall{exec: e, call: call, pre: e.runPreToolHook(ctx, call)}, nil
}

func (c *mcpServerCall) finish(ctx context.Context, result tool.Result, err error) (tool.Result, error) {
	e := c.exec
	e.session.appendMessage("assistant", capText(result.Output))
	err = errors.Join(err, newTaskSessionStore(e.paths).Save(e.session))
	return result, errors.Join(err, e.finish(ctx, err))
}

func (c *mcpServerCall) invoke(ctx context.Context, registry *tool.Registry, approve ApprovalFunc) (tool.Result, error) {
	e := c.exec
	w := e.workspace
	w.SetApprovalFunc(approve)
	w.approvalPending, w.approvalRecord = e.pendingApproval, e.approval
	defer func() { w.SetApprovalFunc(nil); w.approvalPending, w.approvalRecord = nil, nil }()
	result, err := e.invokePlanned(ctx, registry, c.call, c.pre)
	return c.finish(ctx, result, err)
}

func (c *mcpServerCall) challenge(ctx context.Context, message string) error {
	e := c.exec
	kind := ApprovalCommand
	switch c.call.Effect {
	case task.ExternalRead:
		kind = ApprovalExternalRead
	case task.Write:
		kind = ApprovalChanges
	}
	c.review = ApprovalRequest{Source: "human", Kind: kind, Title: "Call " + c.call.Name, Detail: message}
	e.current = &c.call
	defer func() { e.current = nil }()
	if blocked, reason := e.workspace.checkPermissionHook(context.WithValue(ctx, executionContextKey{}, e), c.review); blocked {
		result, err := e.declinePlanned(ctx, c.call, "Declined by PermissionRequest hook: "+reason)
		_, err = c.finish(ctx, result, errors.Join(err, e.err))
		return errors.Join(errors.New(result.Output), err)
	}
	if err := e.db.SetToolReview(ctx, e.lease, c.call, true); err != nil {
		return errors.Join(err, e.finish(ctx, err))
	}
	e.permissionReviewed = true
	return e.closeWithRunEnd(ctx)
}

func (c *mcpServerCall) acquire(ctx context.Context) error {
	e := c.exec
	var err error
	e.db, err = taskstore.Open(taskDirectory(e.paths))
	if err != nil {
		return err
	}
	e.lease, err = e.db.Acquire(ctx, e.session.ID, e.workspace.root)
	if err != nil {
		return errors.Join(err, e.close())
	}
	return nil
}

func (c *mcpServerCall) resumeReview(ctx context.Context) error {
	if err := c.acquire(ctx); err != nil {
		return err
	}
	e := c.exec
	if err := e.db.SetToolReview(ctx, e.lease, c.call, false); err != nil {
		return errors.Join(err, e.close())
	}
	return nil
}

func (c *mcpServerCall) expire(ctx context.Context) error {
	c.expires = time.Time{}
	if err := c.acquire(ctx); err != nil {
		return err
	}
	e := c.exec
	run, err := e.db.GetRun(ctx, e.run.ID)
	if err != nil || run.Status.Terminal() {
		return errors.Join(err, e.close())
	}
	return e.finish(ctx, errors.New("MCP approval expired or invalidated"))
}

func (c *mcpServerCall) decline(ctx context.Context) (tool.Result, error) {
	e := c.exec
	e.current = &c.call
	defer func() { e.current = nil }()
	err := e.pendingApproval(ctx, c.review)
	if err == nil {
		err = e.approval(ctx, c.review, false)
	}
	var result tool.Result
	if err == nil {
		result, err = e.declinePlanned(ctx, c.call, "Declined; operation was not executed.")
	}
	return c.finish(ctx, result, err)
}

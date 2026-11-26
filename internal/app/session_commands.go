package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	taskstore "meldra/internal/store"
	"meldra/internal/task"
)

const initProjectPrompt = `Inspect this repository with read_project_context, list_files, and read_file. Draft a concise root AGENTS.md grounded in the actual build/test commands, code ownership, and project-specific constraints you find. Preserve existing guidance: if AGENTS.md exists, propose targeted improvements rather than replacing it. Do not invent commands or conventions. Use normal edit tools and approvals to save the draft in Build mode; in Plan mode, present the proposed text without writing. Do not commit it unless separately requested.`

func validModelName(model string) bool {
	return len(model) <= 256 && utf8.ValidString(model) && strings.TrimSpace(model) != "" && !strings.ContainsFunc(model, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func (a *Agent) handleSessionCommand(ctx context.Context, kind, value string) (err error) {
	if !a.turnMu.TryLock() {
		return ErrAgentBusy
	}
	defer a.turnMu.Unlock()
	a.emit(UIEvent{Kind: UIEventStatus, Text: "Updating session"})
	if kind == "model" && value == "" {
		a.emitNotice("Current model: " + a.modelName())
		return nil
	}
	if kind == "model" && !validModelName(value) || kind == "fork" && value != "" {
		a.emitNotice("Usage: /model [model] or /fork")
		return nil
	}
	if a.session == nil || a.store == nil || a.execution == nil {
		a.emitNotice("This command requires a saved session")
		return nil
	}
	if kind == "fork" && !a.session.hasSavedRevision {
		a.emitNotice("Start a conversation before forking; no parent session has been saved yet")
		return nil
	}
	c := a.initControl()
	c.mu.Lock()
	pending := len(c.pending) != 0 || len(c.recovered) != 0 || c.selected != nil
	c.mu.Unlock()
	if pending {
		a.emitNotice("Apply or finish pending requests before changing the session")
		return nil
	}
	e := a.execution
	e.requestSequence = 0
	sourceID := a.session.ID
	e.db, err = taskstore.Open(taskDirectory(e.paths))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, e.close()) }()
	e.lease, err = e.db.Acquire(ctx, a.session.ID, a.session.Workspace)
	if err != nil {
		return err
	}
	_, lookupErr := e.db.GetTask(ctx, sourceID)
	var history *compactionArtifact
	if lookupErr == nil {
		history, err = a.compactionHistory(ctx, len(a.session.Messages))
		if err != nil {
			if !errors.Is(err, errCompactionUnresolved) {
				return err
			}
			a.emitNotice("Session unchanged: " + sanitizeTerminalText(err.Error()))
			return nil
		}
		requests, err := e.db.RequestEvents(ctx, sourceID, a.session.LastRequestSequence, 1)
		if err != nil {
			return err
		}
		if len(requests) != 0 {
			a.emitNotice("Resume a normal turn to recover missing requests before changing the session")
			return nil
		}
	} else if !errors.Is(lookupErr, task.ErrNotFound) {
		return lookupErr
	} else if e.resume {
		a.emitNotice("Resume a normal turn to import legacy history before changing the session")
		return nil
	}
	next := *a.session
	if history != nil {
		next.Messages = slices.Clone(a.session.Messages)
		for i := range next.Messages {
			next.Messages[i].Inherited = history.inheritedMessages[i]
		}
	}
	next.taskSnapshot = true
	next.PreviousResponseID = ""
	next.resumed = true
	if kind == "model" {
		if value == a.modelName() {
			a.emitNotice("Current model: " + value)
			return nil
		}
		next.Model = value
		next.ModelContextWindow = 0
		if errors.Is(lookupErr, task.ErrNotFound) {
			if _, err := e.db.EnsureTask(ctx, task.Task{ID: sourceID, SessionID: sourceID, Workspace: next.Workspace, Goal: "Interactive session"}); err != nil {
				return err
			}
		}
	} else {
		if a.session.hasSavedRevision {
			current, err := newTaskSessionStore(e.paths).Load(sourceID)
			if err != nil {
				return err
			}
			if current.savedRevision != a.session.savedRevision {
				return fmt.Errorf("session changed since loading; reload before forking")
			}
		}
		next.ID, err = newSessionID()
		if err != nil {
			return err
		}
		next.ParentSessionID = a.session.ID
		next.CreatedAt = time.Now().UTC()
		next.hasSavedRevision = false
		next.LastRequestSequence = 0
		next.RequestsReplayedWithoutCheckpoint = false
		next.ApprovedPlanDigest = ""
		next.Messages = slices.Clone(a.session.Messages)
		for i := range next.Messages {
			next.Messages[i].Inherited = true
			next.Messages[i].RequestSequence = 0
			next.Messages[i].ControlID = ""
		}
		next.ContextCompaction = nil
		next.CompactedThroughSequence = 0
		next.taskSnapshot = true
		if _, err := e.db.EnsureTask(ctx, task.Task{ID: next.ID, SessionID: next.ID, Workspace: next.Workspace, Goal: "Fork of " + next.ParentSessionID}); err != nil {
			return err
		}
	}
	if err := a.store.Save(&next); err != nil {
		return &persistenceError{err}
	}
	*a.session = next
	a.previousResponseID = ""
	a.observedInputTokens, a.observedContextBytes = 0, 0
	if kind == "model" {
		a.model, e.config.Model = value, value
		e.resume = true
		a.contextWindowTokens = 0
		a.emit(UIEvent{Kind: UIEventModel, Text: value})
		a.emitNotice("Model switched to " + value + " on the current provider; token window reset. Configuration files are unchanged.")
	} else {
		e.resume = false
		if a.workspace != nil {
			a.workspace.undoHistory = nil
		}
		a.emitNotice(fmt.Sprintf("Forked session %s from %s. Workspace files are shared, not copied or rewound.", next.ID, next.ParentSessionID))
		a.startSessionHooks(ctx, "fork")
	}
	data, _ := json.Marshal(map[string]string{"model": a.modelName(), "session_id": next.ID})
	if _, lookupErr := e.db.GetTask(ctx, sourceID); lookupErr == nil {
		return e.db.AppendEvent(ctx, e.lease, task.Event{TaskID: sourceID, Kind: "session." + kind, Data: data})
	} else if !errors.Is(lookupErr, task.ErrNotFound) {
		return lookupErr
	}
	return nil
}

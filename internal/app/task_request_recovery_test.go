package app

import (
	"context"
	"strings"
	"testing"

	"meldra/internal/provider"
)

func TestTaskRecoveryReplaysRequestsBeforeStartingAnotherRun(t *testing.T) {
	agent, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	if err := agent.RunTurn(t.Context(), "create a Go HTTP server"); err != nil {
		t.Fatal(err)
	}
	firstCheckpoint := agent.session.LastRequestSequence
	if firstCheckpoint <= 0 {
		t.Fatal("user snapshot missing event checkpoint")
	}
	const changed = "Actually delete the server and write a Python CLI"
	if err := agent.execution.begin(t.Context(), changed); err != nil {
		t.Fatal(err)
	}
	lostSequence := agent.execution.requestSequence
	if err := agent.execution.close(); err != nil {
		t.Fatal(err)
	}
	session, err := loadSessionForResume(paths, agent.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.LastRequestSequence != firstCheckpoint {
		t.Fatal("fixture unexpectedly saved the second request")
	}
	agent.session = session
	agent.execution.session = session
	agent.execution.resume = true
	if err := agent.execution.begin(t.Context(), "continue"); err != nil {
		t.Fatal(err)
	}
	// Recovery must be durable before the new Run's request snapshot: simulate
	// another crash here and prove the changed instruction survives both exits.
	saved, err := loadSessionForResume(paths, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.LastRequestSequence != lostSequence || !strings.Contains(saved.resumeContext(), changed) {
		t.Fatalf("recovered=%+v", saved)
	}
	if err := agent.execution.close(); err != nil {
		t.Fatal(err)
	}
	agent.session = saved
	agent.execution.session = saved
	agent.execution.resume = true
	if err := agent.RunTurn(t.Context(), "resume explicitly"); err != nil {
		t.Fatal(err)
	}
	final, err := loadSessionForResume(paths, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, message := range final.Messages {
		if message.Role == "user" {
			counts[message.Content]++
		}
	}
	for _, text := range []string{"create a Go HTTP server", changed, "continue", "resume explicitly"} {
		if counts[text] != 1 {
			t.Errorf("request %q occurred %d times", text, counts[text])
		}
	}
}

func TestTaskRecoveryDoesNotDeduplicateSeparateIdenticalRequests(t *testing.T) {
	agent, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	if err := agent.RunTurn(t.Context(), "run tests again"); err != nil {
		t.Fatal(err)
	}
	if err := agent.execution.begin(t.Context(), "run tests again"); err != nil {
		t.Fatal(err)
	}
	if err := agent.execution.close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSessionForResume(paths, agent.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	agent.session = loaded
	agent.execution.session = loaded
	agent.execution.resume = true
	if err := agent.RunTurn(t.Context(), "continue"); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, message := range agent.session.Messages {
		if message.Role == "user" && message.Content == "run tests again" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("different run IDs collapsed into %d request", count)
	}
}

func TestTaskRecoveryUpgradesSnapshotsWithoutRequestCheckpoint(t *testing.T) {
	agent, paths := recordedAgent(t, inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		return provider.Result{Response: finishResponse()}, nil
	}), nil)
	if err := agent.RunTurn(t.Context(), "original"); err != nil {
		t.Fatal(err)
	}
	agent.session.LastRequestSequence = 0
	if err := agent.store.Save(agent.session); err != nil {
		t.Fatal(err)
	}
	if err := agent.execution.begin(t.Context(), "newer request"); err != nil {
		t.Fatal(err)
	}
	if err := agent.execution.close(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSessionForResume(paths, agent.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	agent.session = loaded
	agent.execution.session = loaded
	agent.execution.resume = true
	if err := agent.RunTurn(t.Context(), "continue"); err != nil {
		t.Fatal(err)
	}
	var users []string
	for _, message := range agent.session.Messages {
		if message.Role == "user" {
			users = append(users, message.Content)
		}
	}
	// Without a watermark, preserve durable requests once instead of guessing.
	if strings.Join(users, "|") != "original|original|newer request|continue" {
		t.Fatalf("upgraded user history=%v", users)
	}
	// The lightweight list decoder must understand the new optional field too.
	sessions, diagnostics, err := NewSessionStore(paths).ListWithDiagnostics()
	if err != nil || diagnostics.SkippedFiles != 0 || len(sessions) != 1 {
		t.Fatalf("list=%v diagnostics=%+v err=%v", sessions, diagnostics, err)
	}
}

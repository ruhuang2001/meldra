package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/provider"
	"meldra/internal/task"
)

func TestTaskSessionSaveFailureStopsBatchAndPreservesRecovery(t *testing.T) {
	for _, name := range []string{"update_plan", "save_summary"} {
		for _, stage := range []string{"before_replace", "after_replace"} {
			t.Run(name+"/"+stage, func(t *testing.T) {
				requests := 0
				var agent *Agent
				var savedBefore []byte
				args := `{"steps":["new plan"]}`
				if name == "save_summary" {
					args = `{"summary":"new summary"}`
				}
				fault := errors.New("directory sync failed after snapshot replacement")
				backend := inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
					requests++
					if requests != 1 {
						t.Fatal("model called again after session persistence failed")
					}
					store := agent.store.(*SessionStore)
					if stage == "before_replace" {
						// A concurrent snapshot save must not be overwritten. This
						// exercises a real save failure before atomic replacement.
						external := *agent.session
						external.Summary = "external update"
						if err := store.Save(&external); err != nil {
							t.Fatal(err)
						}
					} else {
						store.syncDir = func(string) error { return fault }
					}
					var err error
					savedBefore, err = os.ReadFile(store.path(agent.session.ID))
					if err != nil {
						t.Fatal(err)
					}
					response := callResponse("save-metadata", name, args)
					response.Output = append(response.Output, provider.OutputItem{
						Type: "function_call", CallID: "next-edit", Name: "edit_file",
						Arguments: `{"path":"must-not-exist.txt","old_str":"","new_str":"unexpected effect"}`,
					})
					return provider.Result{Response: response}, nil
				})
				agent, paths := recordedAgent(t, backend, nil)
				agent.tools = append(agent.tools, NewSessionTools(agent.session, agent.store).ToolDefinitions()...)
				err := agent.RunTurn(t.Context(), "save metadata before editing")
				if _, ok := errors.AsType[*persistenceError](err); !ok {
					t.Fatalf("expected fatal persistence error, got %v", err)
				}
				if stage == "after_replace" && !errors.Is(err, fault) {
					t.Fatalf("lost original save failure: %v", err)
				}
				if requests != 1 {
					t.Fatalf("model requests=%d, want 1", requests)
				}
				if _, err := os.Stat(filepath.Join(agent.execution.workspace.root, "must-not-exist.txt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("next tool ran: %v", err)
				}
				if len(agent.session.Plan) != 0 || agent.session.Summary != "" {
					t.Fatal("failed save published unconfirmed in-memory metadata")
				}
				db := openTaskDB(t, paths)
				calls, err := db.ToolCalls(t.Context(), agent.session.ID)
				wantStatus := task.ToolFailed
				if stage == "after_replace" {
					wantStatus = task.ToolUnknown
				}
				if err != nil || len(calls) != 1 || calls[0].Status != wantStatus || calls[0].Result.Error == "" {
					t.Fatalf("calls=%+v error=%v, want one %s result", calls, err, wantStatus)
				}
				runs, err := db.Runs(t.Context(), agent.session.ID)
				if err != nil || len(runs) != 1 || runs[0].Status != task.RunInterrupted || !strings.Contains(runs[0].Reason, "task recording failed") {
					t.Fatalf("runs=%+v error=%v", runs, err)
				}
				snapshot, err := NewSessionStore(paths).Load(agent.session.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stage == "before_replace" {
					data, err := os.ReadFile(newTaskSessionStore(paths).path(agent.session.ID))
					if err != nil || !bytes.Equal(data, savedBefore) {
						t.Fatal("pre-replacement failure changed the existing snapshot")
					}
				} else if name == "update_plan" && (len(snapshot.Plan) != 1 || snapshot.Plan[0] != "new plan") || name == "save_summary" && snapshot.Summary != "new summary" {
					t.Fatalf("post-replacement failure did not preserve its snapshot evidence: %+v", snapshot)
				}

				// Reload through the actual recovery path. Known failures may resume;
				// an uncertain replacement must block before another model request.
				runtime, err := newChatRuntime(t.Context(), paths, Settings{Model: "fixture", BaseURL: defaultBaseURL}, ChatOptions{Resume: agent.session.ID}, func(root string, auto bool) (*Workspace, error) {
					return NewWorkspace(root, bufio.NewReader(strings.NewReader("")), io.Discard, auto)
				}, nil, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				agent = runtime.agent
				recovered := agent.execution
				resumedRequests := 0
				agent.backend = inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
					requests++
					resumedRequests++
					if resumedRequests == 1 {
						return provider.Result{Response: callResponse("recovered-summary", "save_summary", `{"summary":"recovered handler"}`)}, nil
					}
					return provider.Result{Response: finishResponse()}, nil
				})
				if stage == "after_replace" {
					if err := agent.RunTurn(t.Context(), "continue"); !errors.Is(err, task.ErrUnresolved) || requests != 1 {
						t.Fatalf("uncertain save resumed: error=%v requests=%d", err, requests)
					}
					lease, err := db.Acquire(t.Context(), agent.session.ID, recovered.workspace.root)
					if err != nil {
						t.Fatal(err)
					}
					err = db.ResolveTool(t.Context(), lease, calls[0].ID, task.Result{Status: task.ToolSucceeded}, "inspected saved metadata")
					_ = lease.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := agent.RunTurn(t.Context(), "continue explicitly"); err != nil || requests != 3 {
					t.Fatalf("explicit recovery failed: error=%v requests=%d", err, requests)
				}
				saved, err := NewSessionStore(paths).Load(agent.session.ID)
				if err != nil || saved.Summary != "recovered handler" {
					t.Fatalf("recovered tools saved stale session: %+v %v", saved, err)
				}
				old, err := db.GetRun(t.Context(), runs[0].ID)
				if err != nil || old.Status != task.RunInterrupted {
					t.Fatalf("recovery overwrote original run: %+v %v", old, err)
				}
			})
		}
	}
}

func TestSessionValidationFailureDoesNotStopFollowingTool(t *testing.T) {
	for _, name := range []string{"update_plan", "save_summary"} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			backend := inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
				requests++
				if requests == 1 {
					response := callResponse("invalid-metadata", name, `{}`)
					response.Output = append(response.Output, provider.OutputItem{Type: "function_call", CallID: "read-next", Name: "list_files", Arguments: `{"path":"."}`})
					return provider.Result{Response: response}, nil
				}
				return provider.Result{Response: finishResponse()}, nil
			})
			agent, paths := recordedAgent(t, backend, nil)
			agent.tools = append(agent.tools, NewSessionTools(agent.session, agent.store).ToolDefinitions()...)
			if err := agent.RunTurn(t.Context(), "validate metadata"); err != nil {
				t.Fatal(err)
			}
			calls, err := openTaskDB(t, paths).ToolCalls(t.Context(), agent.session.ID)
			if err != nil || len(calls) != 2 || calls[0].Status != task.ToolFailed || calls[1].Status != task.ToolSucceeded || requests != 2 {
				t.Fatalf("calls=%+v requests=%d error=%v", calls, requests, err)
			}
		})
	}
}

func TestHeadlessSessionSaveFailureStopsFollowingTool(t *testing.T) {
	requests, effects := 0, 0
	session := &Session{ID: "headless"}
	store := saveSessionFunc(func(*Session) error { return errors.New("storage failure with unknown write outcome") })
	definitions := NewSessionTools(session, store).ToolDefinitions()
	definitions = append(definitions, ToolDefinition{Name: "effect", Function: func(context.Context, json.RawMessage) (string, error) {
		effects++
		return "unexpected", nil
	}})
	agent := NewAgent(inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
		requests++
		response := callResponse("save", "save_summary", `{"summary":"pending"}`)
		response.Output = append(response.Output, provider.OutputItem{Type: "function_call", CallID: "next", Name: "effect", Arguments: `{}`})
		return provider.Result{Response: response}, nil
	}), nil, definitions)
	agent.output = io.Discard
	err := agent.RunTurn(t.Context(), "save first")
	if _, ok := errors.AsType[*persistenceError](err); !ok || requests != 1 || effects != 0 || session.Summary != "" {
		t.Fatalf("error=%v requests=%d effects=%d summary=%q", err, requests, effects, session.Summary)
	}
}

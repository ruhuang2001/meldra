package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"meldra/internal/provider"
	"meldra/internal/task"
	"meldra/internal/tool"
)

// Decode fixtures through the public provider boundary so full replay receives
// the original provider items rather than dispatcher-only OutputItem literals.
func steeringBatchResponse(t *testing.T, id string, calls ...map[string]any) *provider.Response {
	t.Helper()
	var output []provider.OutputItem
	for i, call := range calls {
		arguments, err := json.Marshal(call["arguments"])
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(map[string]any{
			"type": "function_call", "id": fmt.Sprintf("fc_%s_%d", id, i),
			"call_id": call["id"], "name": call["name"], "arguments": string(arguments), "status": "completed",
		})
		if err != nil {
			t.Fatal(err)
		}
		var item provider.OutputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		output = append(output, item)
	}
	return &provider.Response{ID: id, Status: "completed", Output: output}
}

func steeringEdit(id, path, content string) map[string]any {
	return map[string]any{"id": id, "name": "edit_file", "arguments": map[string]string{"path": path, "old_str": "", "new_str": content}}
}

func assertSteeringFile(t *testing.T, root, path, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(root, path))
	if err != nil || string(got) != want {
		t.Fatalf("file %s = %q, error = %v; want %q", path, got, err, want)
	}
}

func assertSteeringFilesAbsent(t *testing.T, root string, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(filepath.Join(root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unstarted old-batch file %s exists or could not be inspected: %v", path, err)
		}
	}
}

func TestSteeringAfterFirstPersistedWriteInvalidatesRemainingBatch(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "previous-response-id"
		if custom {
			name = "full-replay"
		}
		t.Run(name, func(t *testing.T) {
			const goal = "Create the requested files using the original task."
			const correction = "Keep the first completed file; create corrected.txt and skip the other original files."
			a, paths := recordedAgent(t, nil, nil)
			w := a.execution.workspace
			a.workspace, w.policy, a.customProvider = w, a.policy, custom
			requests, corrections := 0, 0
			a.backend = inferenceFunc(func(ctx context.Context, request provider.Request, options provider.Options, _ provider.Observer) (provider.Result, error) {
				requests++
				if options.CustomProvider != custom {
					t.Fatalf("provider mode = %t, want %t", options.CustomProvider, custom)
				}
				switch requests {
				case 1:
					return provider.Result{Response: steeringBatchResponse(t, "original-three-writes",
						steeringEdit("first", "first.txt", "FIRST_PERSISTED_EFFECT"),
						steeringEdit("second", "second.txt", "STALE_SECOND_EFFECT"),
						steeringEdit("third", "third.txt", "STALE_THIRD_EFFECT"))}, nil
				case 2:
					if request.PreviousResponseID != "" {
						t.Fatalf("steering retained superseded continuation %q", request.PreviousResponseID)
					}
					// Input deliberately hides SDK types. Its diagnostic value exposes
					// the actual supplied text without accessing provider-private fields.
					input := fmt.Sprintf("%+v", request.Input)
					for _, evidence := range []string{goal, correction, "edit_file: succeeded", "FIRST_PERSISTED_EFFECT"} {
						if !strings.Contains(input, evidence) {
							t.Fatalf("corrected model request omitted %q: %s", evidence, input)
						}
					}
					assertSteeringFile(t, w.root, "first.txt", "FIRST_PERSISTED_EFFECT")
					assertSteeringFilesAbsent(t, w.root, "second.txt", "third.txt")
					calls, err := a.execution.db.ToolCalls(ctx, a.session.ID)
					if err != nil || len(calls) != 1 || calls[0].ProviderCallID != "first" || calls[0].Status != task.ToolSucceeded {
						t.Fatalf("old batch admitted additional calls: %+v, error = %v", calls, err)
					}
					return provider.Result{Response: steeringBatchResponse(t, "corrected-response", steeringEdit("corrected", "corrected.txt", "CORRECTION_FOLLOWED"))}, nil
				case 3:
					wantPrevious := "corrected-response"
					if custom {
						wantPrevious = ""
					}
					if request.PreviousResponseID != wantPrevious {
						t.Fatalf("corrected response continuation = %q, want %q", request.PreviousResponseID, wantPrevious)
					}
					assertSteeringFile(t, w.root, "corrected.txt", "CORRECTION_FOLLOWED")
					return provider.Result{Response: finishResponse()}, nil
				default:
					t.Fatalf("unexpected model request %d", requests)
					return provider.Result{}, nil
				}
			})
			a.events = UIEventSinkFunc(func(event UIEvent) {
				if event.Kind != UIEventToolFinished || event.Name != "edit_file" || corrections != 0 {
					return
				}
				calls, err := a.execution.db.ToolCalls(t.Context(), a.session.ID)
				if err != nil || len(calls) != 1 || calls[0].Status != task.ToolSucceeded {
					t.Fatalf("UI boundary precedes durable first result: %+v, error = %v", calls, err)
				}
				assertSteeringFile(t, w.root, "first.txt", "FIRST_PERSISTED_EFFECT")
				corrections++
				if err := a.SubmitControl("steer", correction); err != nil {
					t.Fatal(err)
				}
			})
			if err := a.RunTurn(t.Context(), goal); err != nil {
				t.Fatal(err)
			}
			if requests != 3 || corrections != 1 {
				t.Fatalf("requests = %d, corrections = %d", requests, corrections)
			}
			assertSteeringFile(t, w.root, "first.txt", "FIRST_PERSISTED_EFFECT")
			assertSteeringFilesAbsent(t, w.root, "second.txt", "third.txt")
			db := openTaskDB(t, paths)
			calls, err := db.ToolCalls(t.Context(), a.session.ID)
			if err != nil || len(calls) != 2 {
				t.Fatalf("persisted calls = %+v, error = %v", calls, err)
			}
			for _, call := range calls {
				if call.Status != task.ToolSucceeded || call.ProviderCallID != "first" && call.ProviderCallID != "corrected" {
					t.Fatalf("unexpected persisted call: %+v", call)
				}
			}
		})
	}
}

func TestSteeringCannotContinueBatchAfterUnknownEffect(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "previous-response-id"
		if custom {
			name = "full-replay"
		}
		t.Run(name, func(t *testing.T) {
			a, paths := recordedAgent(t, nil, nil)
			w := a.execution.workspace
			a.workspace, w.policy, a.customProvider = w, a.policy, custom
			requests, effects, corrections := 0, 0, 0
			a.tools = append(a.tools, ToolDefinition{Name: "uncertain_external", Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
				effects++
				tool.Observe(ctx, func(observation *tool.Observation) { observation.Started = true })
				if err := os.WriteFile(filepath.Join(w.root, "observed-effect.txt"), []byte("EFFECT_MUST_NOT_REPLAY"), 0o600); err != nil {
					return "", err
				}
				return "External effect occurred, completion confirmation was lost.", errors.New("connection lost after dispatch")
			}})
			a.backend = inferenceFunc(func(context.Context, provider.Request, provider.Options, provider.Observer) (provider.Result, error) {
				requests++
				if requests != 1 {
					t.Fatal("model continued despite an unresolved outcome")
				}
				return provider.Result{Response: steeringBatchResponse(t, "unknown-three-calls",
					map[string]any{"id": "unknown", "name": "uncertain_external", "arguments": map[string]any{}},
					steeringEdit("second", "second.txt", "STALE_SECOND_EFFECT"),
					steeringEdit("third", "third.txt", "STALE_THIRD_EFFECT"))}, nil
			})
			a.events = UIEventSinkFunc(func(event UIEvent) {
				if event.Kind != UIEventToolFinished || event.Name != "uncertain_external" {
					return
				}
				calls, err := a.execution.db.ToolCalls(t.Context(), a.session.ID)
				if err != nil || len(calls) != 1 || calls[0].Status != task.ToolUnknown {
					t.Fatalf("unknown boundary not persisted: %+v, error = %v", calls, err)
				}
				corrections++
				if err := a.SubmitControl("steer", "Continue only after checking the uncertain operation."); err != nil {
					t.Fatal(err)
				}
			})
			if err := a.RunTurn(t.Context(), "Run the external action and related edits."); err == nil || !strings.Contains(err.Error(), "unknown outcome") {
				t.Fatalf("unknown outcome did not stop turn: %v", err)
			}
			assertSteeringFilesAbsent(t, w.root, "second.txt", "third.txt")
			assertSteeringFile(t, w.root, "observed-effect.txt", "EFFECT_MUST_NOT_REPLAY")
			db := openTaskDB(t, paths)
			calls, err := db.ToolCalls(t.Context(), a.session.ID)
			if err != nil || len(calls) != 1 || calls[0].Status != task.ToolUnknown {
				t.Fatalf("unknown result changed: %+v, error = %v", calls, err)
			}
			events, err := db.Events(t.Context(), a.session.ID, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			received, applied := 0, 0
			for _, event := range events {
				switch event.Kind {
				case "control.received":
					received++
				case "control.applied":
					applied++
				}
			}
			if received != 1 || applied != 0 {
				t.Fatalf("correction should remain durable but unapplied: received = %d, applied = %d", received, applied)
			}
			a.execution.resume = true
			if err := a.RunTurn(t.Context(), "Resume without reconciling the unknown effect."); !errors.Is(err, task.ErrUnresolved) {
				t.Fatalf("resume bypassed unknown-effect gate: %v", err)
			}
			if requests != 1 || effects != 1 || corrections != 1 {
				t.Fatalf("requests = %d, effects = %d, corrections = %d", requests, effects, corrections)
			}
		})
	}
}

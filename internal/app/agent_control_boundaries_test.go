package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"meldra/internal/provider"
	"meldra/internal/task"
)

func TestRunStopPreservesInterruptionSaveFailure(t *testing.T) {
	saveErr := errors.New("interruption snapshot could not be saved")
	a := NewAgent(nil, nil, nil)
	a.session = &Session{ID: "stop-save", Workspace: t.TempDir()}
	saves := 0
	a.store = saveSessionFunc(func(*Session) error {
		saves++
		if saves > 1 {
			return saveErr
		}
		return nil
	})
	reads := 0
	a.getUserMessage = func() (string, bool) {
		reads++
		return "stop this request", reads == 1
	}
	a.backend = inferenceFunc(func(ctx context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		if err := a.SubmitControl("stop", ""); err != nil {
			t.Fatal(err)
		}
		return provider.Result{}, ctx.Err()
	})
	err := a.Run(t.Context())
	if !errors.Is(err, saveErr) || !errors.Is(err, errTurnStopped) {
		t.Fatalf("stopped Run hid the snapshot failure: %v", err)
	}
	if reads != 1 || saves != 2 {
		t.Fatalf("Run continued after snapshot failure: reads=%d saves=%d", reads, saves)
	}
}

func TestRunStopPreservesUnresolvedProcessCleanup(t *testing.T) {
	a := NewAgent(nil, nil, nil)
	a.workspace = contextWorkspace(t)
	reads := 0
	a.getUserMessage = func() (string, bool) {
		reads++
		return "stop owned work", reads == 1
	}
	a.backend = inferenceFunc(func(ctx context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		// A reaped process with uncertain effects must still block continuation.
		done := make(chan struct{})
		close(done)
		m := a.workspace.processes
		m.mu.Lock()
		m.processes["uncertain-process"] = &managedProcess{record: task.Process{ID: "uncertain-process", Effects: "unknown"}, done: done}
		m.mu.Unlock()
		if err := a.SubmitControl("stop", ""); err != nil {
			t.Fatal(err)
		}
		return provider.Result{}, ctx.Err()
	})
	err := a.Run(t.Context())
	if !errors.Is(err, task.ErrUnresolved) || !errors.Is(err, errTurnStopped) {
		t.Fatalf("stopped Run hid unresolved cleanup: %v", err)
	}
	if reads != 1 {
		t.Fatalf("Run read new work after unresolved cleanup: %d reads", reads)
	}
}

func TestRunContinuesAfterSuccessfulStop(t *testing.T) {
	a := NewAgent(nil, userMessages("first", "second"), nil)
	requests := 0
	a.backend = inferenceFunc(func(ctx context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		requests++
		if requests == 1 {
			if err := a.SubmitControl("stop", ""); err != nil {
				t.Fatal(err)
			}
			return provider.Result{}, ctx.Err()
		}
		return provider.Result{Response: finishResponse()}, nil
	})
	if err := a.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("successful stop prevented later work: %d requests", requests)
	}
}

func TestRunCancellationPreservesJoinedProcessCleanupFailure(t *testing.T) {
	cleanupErr := errors.New("process lifecycle could not be saved")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a := NewAgent(nil, userMessages("cancel owned work"), nil)
	a.workspace = contextWorkspace(t)
	a.backend = inferenceFunc(func(inferCtx context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
		cancel()
		m := a.workspace.processes
		m.mu.Lock()
		m.err = errors.Join(inferCtx.Err(), cleanupErr)
		m.mu.Unlock()
		return provider.Result{}, inferCtx.Err()
	})
	if err := a.Run(ctx); !errors.Is(err, cleanupErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Run hid joined cleanup failure: %v", err)
	}
}

func TestSteeringRejectsCustomProviderContextOverBudget(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		for _, withSession := range []bool{false, true} {
			t.Run(fmt.Sprintf("interrupted=%t/session=%t", interrupted, withSession), func(t *testing.T) {
				a := NewAgent(nil, nil, nil)
				a.customProvider = true
				a.maxCustomTurnInputBytes = 1024
				if withSession {
					a.session = &Session{ID: "steer-budget", Workspace: t.TempDir()}
					a.store = saveSessionFunc(func(*Session) error { return nil })
				}
				requests := 0
				a.backend = inferenceFunc(func(ctx context.Context, _ provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
					requests++
					if requests == 1 {
						if err := a.SubmitControl("steer", strings.Repeat("correction", 256)); err != nil {
							t.Fatal(err)
						}
						if interrupted {
							return provider.Result{}, ctx.Err()
						}
					}
					return provider.Result{Response: finishResponse()}, nil
				})
				err := a.RunTurn(t.Context(), "original request")
				if err == nil || !strings.Contains(err.Error(), "custom-provider context exceeds 1024 byte limit") {
					t.Fatalf("over-budget steering was accepted: %v", err)
				}
				if requests != 1 {
					t.Fatalf("over-budget context reached the provider: %d requests", requests)
				}
			})
		}
	}
}

func TestSteeringUpdatesCustomProviderContextMetrics(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(fmt.Sprintf("interrupted=%t", interrupted), func(t *testing.T) {
			const goal = "original request"
			const correction = "inspect the corrected request"
			a := NewAgent(nil, nil, nil)
			a.customProvider = true
			a.maxCustomTurnInputBytes = 1024
			var metrics UIMetrics
			a.events = UIEventSinkFunc(func(event UIEvent) {
				if event.Kind == UIEventMetrics && event.Metrics != nil {
					metrics = *event.Metrics
				}
			})
			_, expectedSize, _, err := provider.BoundInput(provider.UserItems(goal+"\nUser correction:\n"+correction+"\n"), a.customTurnInputLimit())
			if err != nil {
				t.Fatal(err)
			}
			requests := 0
			a.backend = inferenceFunc(func(ctx context.Context, request provider.Request, _ provider.Options, _ provider.Observer) (provider.Result, error) {
				requests++
				if requests == 1 {
					if err := a.SubmitControl("steer", correction); err != nil {
						t.Fatal(err)
					}
					if interrupted {
						return provider.Result{}, ctx.Err()
					}
				} else {
					if metrics.ContextBytes != expectedSize {
						t.Errorf("steered context metric=%d; want %d", metrics.ContextBytes, expectedSize)
					}
					if !strings.Contains(fmt.Sprintf("%+v", request.Input), correction) || request.PreviousResponseID != "" {
						t.Error("steered request lost correction or retained the old continuation")
					}
				}
				return provider.Result{Response: finishResponse()}, nil
			})
			if err := a.RunTurn(t.Context(), goal); err != nil {
				t.Fatal(err)
			}
			if requests != 2 {
				t.Fatalf("requests=%d; want 2", requests)
			}
		})
	}
}

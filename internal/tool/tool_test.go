package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestRegistryRejectsAmbiguousOrMissingHandlers(t *testing.T) {
	handler := func(context.Context, json.RawMessage) (string, error) { return "ok", nil }
	for _, definitions := range [][]Definition{
		{{Name: "", Function: handler}},
		{{Name: "missing"}},
		{{Name: "same", Function: handler}, {Name: "same", Function: handler}},
	} {
		if _, err := New(definitions); err == nil {
			t.Fatalf("accepted invalid definitions: %#v", definitions)
		}
	}
}

func TestRegistryForwardsContextAndRejectsCancelledCalls(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithCancel(t.Context())
	sentinel := errors.New("tool failure")
	registry, err := New([]Definition{{Name: "test", Function: func(got context.Context, input json.RawMessage) (string, error) {
		calls++
		if got != ctx || string(input) != `{"value":1}` {
			t.Fatal("call context or arguments changed")
		}
		return "partial", sentinel
	}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Execute(ctx, "test", json.RawMessage(`{"value":1}`))
	if result != "partial" || !errors.Is(err, sentinel) {
		t.Fatalf("result = %q, error = %v", result, err)
	}
	cancel()
	if _, err := registry.Execute(ctx, "test", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call = %v", err)
	}
	if calls != 1 {
		t.Fatalf("handler executed %d times", calls)
	}
	if _, err := registry.Execute(t.Context(), "unknown", nil); err == nil {
		t.Fatal("unknown tool succeeded")
	}
}

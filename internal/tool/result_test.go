package tool

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestInvokeReportsSemanticOutcomeWithoutParsingText(t *testing.T) {
	definitions := []Definition{
		{Name: "nonzero", Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
			Observe(ctx, func(o *Observation) { o.Result.Status = Failed; o.Result.ExitCode = new(17); o.Result.Truncated = true })
			return "success appears in arbitrary output", nil
		}},
		{Name: "declined", Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
			Observe(ctx, func(o *Observation) { o.Result.Status = Declined })
			return "custom rejection text", nil
		}},
		{Name: "uncertain", Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
			Observe(ctx, func(o *Observation) { o.Started = true })
			return "partial", errors.New("lost process")
		}},
	}
	registry, err := New(definitions)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := registry.Invoke(t.Context(), "nonzero", nil)
	if err != nil || failed.Status != Failed || failed.ExitCode == nil || *failed.ExitCode != 17 || !failed.Truncated {
		t.Fatalf("result=%+v err=%v", failed, err)
	}
	declined, err := registry.Invoke(t.Context(), "declined", nil)
	if err != nil || declined.Status != Declined {
		t.Fatalf("result=%+v err=%v", declined, err)
	}
	unknown, err := registry.Invoke(t.Context(), "uncertain", nil)
	if err == nil || unknown.Status != Unknown || unknown.Output != "partial" || unknown.Retryable {
		t.Fatalf("result=%+v err=%v", unknown, err)
	}
}

func TestSuccessfulObservationCannotHideReturnedError(t *testing.T) {
	for _, started := range []bool{false, true} {
		r, err := New([]Definition{{Name: "ambiguous", Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
			Observe(ctx, func(o *Observation) { o.Started = started; o.Result.Status = Succeeded })
			return "partial", errors.New("later failure")
		}}})
		if err != nil {
			t.Fatal(err)
		}
		result, err := r.Invoke(t.Context(), "ambiguous", nil)
		want := Failed
		if started {
			want = Unknown
		}
		if err == nil || result.Status != want {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}
func TestInvokePropagatesPersistenceFailureAndCancellation(t *testing.T) {
	failure := errors.New("storage failure")
	registry, err := New([]Definition{{Name: "test", Function: func(ctx context.Context, _ json.RawMessage) (string, error) {
		Observe(ctx, func(o *Observation) { o.Err = failure })
		return "not applied", nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Invoke(t.Context(), "test", nil)
	if !errors.Is(err, failure) || result.Status != Failed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err = registry.Invoke(ctx, "test", nil)
	if !errors.Is(err, context.Canceled) || result.Status != Cancelled {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

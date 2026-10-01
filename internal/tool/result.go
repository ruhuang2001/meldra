package tool

import (
	"context"
	"errors"
	"time"
)

const (
	Succeeded = "succeeded"
	Failed    = "failed"
	Declined  = "declined"
	Cancelled = "cancelled"
	Unknown   = "unknown"
)

// Result describes execution independently of the model-facing text. An unknown
// effect must be reconciled, never retried automatically. DurationMS is wall time.
type Result struct {
	Status     string `json:"status"`
	Output     string `json:"output"`
	Error      string `json:"error,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated"`
	Retryable  bool   `json:"retryable"`
}

// Observation lets composite built-in handlers report outcomes at the point of
// execution without parsing their human-readable output. It belongs to one
// synchronous invocation; handlers must not retain it after returning.
type Observation struct {
	Result  Result
	Started bool
	Err     error
}
type observationKey struct{}

func Observe(ctx context.Context, update func(*Observation)) {
	if observation, ok := ctx.Value(observationKey{}).(*Observation); ok {
		update(observation)
	}
}

func resultFor(ctx context.Context, started time.Time, observation *Observation, output string, err error) (Result, error) {
	if observation.Err != nil {
		err = errors.Join(err, observation.Err)
	}
	result := observation.Result
	result.Output = output
	result.DurationMS = time.Since(started).Milliseconds()
	if result.Status == "" {
		result.Status = Succeeded
		if err != nil {
			result.Status = Failed
			if observation.Started {
				result.Status = Unknown
			}
			if errors.Is(err, context.Canceled) && !observation.Started {
				result.Status = Cancelled
			}
		}
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result, err
}

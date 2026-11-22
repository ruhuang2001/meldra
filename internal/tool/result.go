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

type OutputArtifact struct {
	Name      string
	Content   []byte
	Truncated bool
}

type Result struct {
	Attachments []OutputArtifact `json:"-"`
	Status      string           `json:"status"`
	Output      string           `json:"output"`
	Error       string           `json:"error,omitempty"`
	ExitCode    *int             `json:"exit_code,omitempty"`
	DurationMS  int64            `json:"duration_ms"`
	Truncated   bool             `json:"truncated"`
	Retryable   bool             `json:"retryable"`
}

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
	if result.Status == "" || (err != nil && result.Status == Succeeded) {
		result.Status = Succeeded
		if err != nil {
			result.Status = Failed
			if observation.Started {
				result.Status = Unknown
			}
			if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && !observation.Started {
				result.Status = Cancelled
			}
		}
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result, err
}

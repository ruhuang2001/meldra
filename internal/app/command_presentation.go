package app

import (
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
)

// Drain workers never block on the terminal. The full bounded process log is
// separate; coalesced presentation updates may be skipped under backpressure.
func (a *Agent) startCommandPresentation(ctx context.Context) func() {
	if a.workspace == nil {
		return func() {}
	}
	var write func(context.Context, []byte) (int, error)
	if a.events == nil {
		if output, ok := a.writer().(*synchronizedWriter); ok && output.live {
			write = output.writeLive
		} else if a.writer() == io.Discard {
			write = func(_ context.Context, data []byte) (int, error) { return len(data), nil }
		} else {
			// Do not create an unjoinable worker around arbitrary embedding IO.
			// Final command results remain available through the normal interface.
			a.workspace.SetCommandOutput(nil)
			return func() {}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	updates := make(chan CommandOutput, 32)
	done := make(chan struct{})
	var dropped atomic.Bool
	a.workspace.SetCommandOutput(func(update CommandOutput) {
		select {
		case updates <- update:
		default:
			dropped.Store(true)
		}
	})
	go func() {
		defer close(done)
		for {
			if ctx.Err() != nil {
				return
			}
			var update CommandOutput
			select {
			case <-ctx.Done():
				return
			case update = <-updates:
			}
			if dropped.Swap(false) {
				if a.events != nil {
					a.emit(UIEvent{Kind: UIEventNotice, Text: "Live command output skipped; inspect the retained command log."})
				} else if _, err := write(ctx, []byte("Live command output skipped; inspect the retained command log.\n")); err != nil {
					if errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil {
						dropped.Store(true)
						continue
					}
					return
				}
			}
			if a.events != nil {
				detail := ""
				if update.Truncated {
					detail = "truncated"
				}
				a.emit(UIEvent{Kind: UIEventCommandOutput, Name: update.ProcessID, Text: update.Text, Detail: detail})
			} else {
				if _, err := write(ctx, []byte(update.Text)); err != nil {
					if errors.Is(err, os.ErrDeadlineExceeded) && ctx.Err() == nil {
						dropped.Store(true)
						continue
					}
					return
				}
			}
		}
	}()
	return func() { a.workspace.SetCommandOutput(nil); cancel(); <-done }
}

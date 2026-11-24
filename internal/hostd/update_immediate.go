package hostd

import (
	"context"
	"errors"
	"time"
)

// ImmediateUpdateResult contains only fixed reason codes and observed work counts.
// CountsKnown distinguishes observed zero counts from missing counts.
type ImmediateUpdateResult struct {
	Reason       string
	WaitReason   string
	Implementing int
	Publishing   int
	CountsKnown  bool
	ReportStatus string
}

// ImmediateUpdate advances the normal update state under the caller's existing
// hostd lock. The deadline bounds polling and ready confirmation; in-flight
// update and recovery retain their normal safety rules. Saved waiting state is reusable by
// the next normal cycle. It does not execute host commands or other stages.
// On error, the result is empty.
func (r *Runtime) ImmediateUpdate(ctx context.Context, deadline time.Time) (ImmediateUpdateResult, error) {
	return r.ImmediateUpdateWithProgress(ctx, deadline, nil)
}

// ImmediateUpdateWithProgress reports each waiting observation without exposing credentials.
func (r *Runtime) ImmediateUpdateWithProgress(ctx context.Context, deadline time.Time, progress func(ImmediateUpdateResult)) (ImmediateUpdateResult, error) {
	var result ImmediateUpdateResult
	if deadline.IsZero() {
		return result, errors.New("待機期限が必要です")
	}
	r.failure = nil
	for {
		if err := ctx.Err(); err != nil {
			return ImmediateUpdateResult{}, err
		}
		if !r.Now().Before(deadline) {
			result.WaitReason = result.Reason
			result.Reason = "wait_timeout"
			return result, nil
		}
		result = ImmediateUpdateResult{}
		if err := r.updateStep(ctx, &result, deadline); err != nil {
			if ctx.Err() != nil {
				return ImmediateUpdateResult{}, ctx.Err()
			}
			return ImmediateUpdateResult{}, err
		}
		switch result.Reason {
		case "lazy_wait", "heartbeat_wait", "busy":
			if progress != nil {
				progress(result)
			}
		default:
			return result, nil
		}
		remaining := deadline.Sub(r.Now())
		if remaining <= 0 {
			result.WaitReason = result.Reason
			result.Reason = "wait_timeout"
			return result, nil
		}
		if remaining > 5*time.Second {
			remaining = 5 * time.Second
		}
		if err := r.Sleep(ctx, remaining); err != nil {
			if ctx.Err() != nil {
				return ImmediateUpdateResult{}, ctx.Err()
			}
			return ImmediateUpdateResult{}, err
		}
	}
}

package hostd

import (
	"context"
	"strings"
	"time"
)

// An install may name a release other than desired. Releases carry a 12-digit
// revision prefix, while heartbeat carries the full embedded commit.
func targetRevision(h *Heartbeat, t Target) bool {
	if h == nil || !shaPattern.MatchString(h.Revision) {
		return false
	}
	if t.Revision != "" {
		return h.Revision == t.Revision
	}
	return tagPattern.MatchString(t.Tag) && strings.HasPrefix(h.Revision, t.Tag[:12])
}

func (r *Runtime) waitReady(ctx context.Context, s *UpdateState) bool {
	reason, _ := r.waitReadyUntil(ctx, s, time.Time{}, false, nil)
	return reason == "ready"
}

func (r *Runtime) waitReadyUntil(ctx context.Context, s *UpdateState, limit time.Time, persist bool, result *ImmediateUpdateResult) (string, error) {
	deadline := r.Now().Add(time.Duration(r.Config.ReadySeconds) * time.Second)
	if persist {
		if s.ReadyDeadline.IsZero() {
			s.ReadyDeadline = deadline
			if err := r.save("state.json", s); err != nil {
				return "", err
			}
		}
		deadline = s.ReadyDeadline
	}
	// A persisted deadline may expire while no hostd process is running. On
	// resumption, observe the current heartbeat once before declaring failure.
	checkExpired := persist && !r.Now().Before(deadline)
	for {
		if ctx.Err() != nil {
			return "cancelled", nil
		}
		if !limit.IsZero() && !r.Now().Before(limit) && limit.Before(deadline) {
			return "wait_timeout", nil
		}
		if !r.Now().Before(deadline) && !checkExpired {
			return "ready_failed", nil
		}
		current, err := r.environment()
		if err != nil {
			return "ready_failed", nil
		}
		next, err := r.snapshot(ctx, current)
		if err != nil {
			if ctx.Err() != nil {
				return "cancelled", nil
			}
			return "ready_failed", nil
		}
		if result != nil {
			result.CountsKnown = numeric(next.Heartbeat)
			if result.CountsKnown {
				result.Implementing = *next.Heartbeat.Implementing
				result.Publishing = *next.Heartbeat.Publishing
			}
		}
		valid := fresh(next.Heartbeat, s.UpdateStarted, r.Now()) && targetRevision(next.Heartbeat, s.Target)
		if (checkExpired || !r.Now().After(deadline)) && valid && next.Heartbeat.Ready {
			return "ready", nil
		}
		if checkExpired {
			return "ready_failed", nil
		}
		// Preserve the normal update's wait for a worker finishing owned work.
		if valid && numeric(next.Heartbeat) && (*next.Heartbeat.Implementing > 0 || *next.Heartbeat.Publishing > 0) {
			deadline = r.Now().Add(time.Duration(r.Config.ReadySeconds) * time.Second)
			if persist && !deadline.Equal(s.ReadyDeadline) {
				s.ReadyDeadline = deadline
				if err := r.save("state.json", s); err != nil {
					return "", err
				}
			}
		}
		remaining := deadline.Sub(r.Now())
		if !limit.IsZero() && limit.Sub(r.Now()) < remaining {
			remaining = limit.Sub(r.Now())
		}
		if remaining <= 0 {
			continue
		}
		if remaining > 5*time.Second {
			remaining = 5 * time.Second
		}
		if r.Sleep(ctx, remaining) != nil {
			if ctx.Err() != nil {
				return "cancelled", nil
			}
			return "ready_failed", nil
		}
	}
}

// finishReady resumes only after the switch and digest check were persisted.
func (r *Runtime) finishReady(ctx context.Context, state *UpdateState, result *ImmediateUpdateResult, limit time.Time) error {
	reason, err := r.waitReadyUntil(ctx, state, limit, true, result)
	if err != nil {
		return err
	}
	if result != nil {
		result.Reason = reason
	}
	switch reason {
	case "cancelled":
		return ctx.Err()
	case "wait_timeout":
		if result != nil {
			result.WaitReason = "heartbeat_wait"
		}
		return nil
	case "ready":
		state.Phase, state.Status = "reporting", "success"
		if err := r.save("state.json", state); err != nil {
			return err
		}
		if err := r.finishUpdate(ctx, state); err != nil {
			return err
		}
		if result != nil {
			result.Reason = "success"
		}
		return nil
	default:
		if r.failure == nil {
			r.failure = &cycleFailure{"swap-start", "timeout", "ready_timeout"}
		}
		return r.recoverUpdate(context.WithoutCancel(ctx), state)
	}
}

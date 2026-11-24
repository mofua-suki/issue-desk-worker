package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"issue-desk-worker/internal/intake"
)

var ErrLeaseExpired = errors.New("ジョブのleaseが失効しました")

// leaseGuard serializes periodic renewal and publication fences. The independent
// watchdog can cancel execution even while a request owns the renewal slot.
// No uncertain response can extend the last confirmed deadline.
type leaseGuard struct {
	mu            sync.Mutex
	lease         intake.Lease
	failure       error
	failureReason string
	failureAt     time.Time
	job           intake.Job
	api           API
	now           func() time.Time
	ctx           context.Context
	cancel        context.CancelFunc
	slot          chan struct{}
	changed       chan struct{}
	done          chan struct{}
	event         func(int64, int64, string, string)
	events        *leaseEvents
}

func newLeaseGuard(r Runner, ctx context.Context, cancel context.CancelFunc, a intake.Assignment) *leaseGuard {
	g := &leaseGuard{lease: *a.Lease, job: *a.Job, api: r.API, now: r.Now, ctx: ctx, cancel: cancel,
		slot: make(chan struct{}, 1), changed: make(chan struct{}, 1), done: make(chan struct{})}
	events := r.leaseEvents
	if events == nil {
		// Direct execute/guard callers own their diagnostic queue. Run shares
		// one queue across jobs, bounding resources even if a sink never returns.
		g.events = newLeaseEvents(r.leaseEvent, r.Now)
		events = g.events
	}
	g.event = events.record
	go g.watch()
	return g
}

// All callers hold mu. A failure is sticky and cancellation precedes returning.
func (g *leaseGuard) fail(err error, reason string) error {
	if g.failure == nil {
		g.failure = err
		g.failureReason = reason
		g.failureAt = g.now().UTC()
		g.event(g.job.ID, g.lease.Epoch, "stopped", reason)
		g.cancel()
	}
	return g.failure
}

// lost stops the job because the intake answered a request with lease_lost,
// so the reason is recorded like a renewal that was refused the same way.
func (g *leaseGuard) lost(err error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fail(err, "lease_lost")
}

func (g *leaseGuard) current() (intake.Lease, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.failure == nil && !g.lease.Valid(g.now()) {
		g.fail(ErrLeaseExpired, "lease_expired")
	}
	return g.lease, g.failure
}

func (g *leaseGuard) watch() {
	defer close(g.done)
	if g.events != nil {
		defer g.events.close()
	}
	for {
		lease, err := g.current()
		if err != nil {
			return
		}
		timer := time.NewTimer(lease.ExpiresAt.Sub(g.now()))
		select {
		case <-g.ctx.Done():
			timer.Stop()
			return
		case <-g.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (g *leaseGuard) renew(ctx context.Context) error {
	// Waiting for another renewal is cancelable by both caller and job lifetime.
	linked, stop := context.WithCancel(ctx)
	unlink := context.AfterFunc(g.ctx, stop)
	defer func() { unlink(); stop() }()
	select {
	case g.slot <- struct{}{}:
		defer func() { <-g.slot }()
	case <-linked.Done():
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := g.current()
		if err != nil {
			return err
		}
		return g.ctx.Err()
	}
	backoff := time.Second
	lastReason := ""
	for {
		// Recheck cancellation and the latest deadline after obtaining the slot,
		// after every request, and after every retry wait.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lease, err := g.current()
		if err != nil {
			return err
		}
		if g.ctx.Err() != nil {
			return g.ctx.Err()
		}
		requestCtx, finish := context.WithTimeout(linked, min(30*time.Second, lease.ExpiresAt.Sub(g.now())))
		out, err := g.api.Renew(requestCtx, g.job.ID, lease.Epoch)
		if err == nil {
			err = requestCtx.Err()
		}
		finish()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		g.mu.Lock()
		if g.failure != nil {
			err = g.failure
		} else if !g.lease.Valid(g.now()) {
			err = g.fail(ErrLeaseExpired, "lease_expired")
		} else if g.ctx.Err() != nil {
			err = g.ctx.Err()
		} else if err == nil {
			// Renewal need not return the full Next descriptor. Repo and the
			// duplicate job epoch are checked when present; identity, lease epoch
			// and a valid, non-decreasing deadline are always required.
			if out.Job == nil || out.Job.ID != g.job.ID || out.Lease == nil || !out.Lease.Valid(g.now()) ||
				(out.Job.Repo != "" && !strings.EqualFold(out.Job.Repo, g.job.Repo)) ||
				out.Lease.Epoch != lease.Epoch || (out.Job.LeaseEpoch != 0 && out.Job.LeaseEpoch != lease.Epoch) || out.Lease.ExpiresAt.Before(lease.ExpiresAt) {
				err = g.fail(errors.New("lease延長応答が所有中の仕事と一致しません"), "response_mismatch")
			} else {
				g.lease = *out.Lease
				select {
				case g.changed <- struct{}{}:
				default:
				}
				if lastReason != "" {
					g.event(g.job.ID, lease.Epoch, "recovered", lastReason)
				}
			}
		} else if intake.TemporaryFailureReason(err) == "" {
			reason := "renew_rejected"
			var api *intake.APIError
			if errors.As(err, &api) && api.LeaseLost() {
				reason = "lease_lost"
			}
			err = g.fail(err, reason)
		}
		failed := g.failure != nil || g.ctx.Err() != nil
		g.mu.Unlock()
		if err == nil || failed {
			return err
		}
		lastReason = intake.TemporaryFailureReason(err)
		g.event(g.job.ID, lease.Epoch, "temporary_failure", lastReason)
		timer := time.NewTimer(min(backoff, lease.ExpiresAt.Sub(g.now())))
		select {
		case <-linked.Done():
			timer.Stop()
		case <-timer.C:
		}
		backoff = min(2*backoff, 10*time.Second)
	}
}

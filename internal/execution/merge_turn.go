package execution

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

type mergeTurn struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	executor  *Executor
	route     string
	who       map[string]any
	lost      atomic.Bool
	mu        sync.Mutex
	expiresAt time.Time
	epoch     int64
	slot      chan struct{}
	changed   chan struct{}
	watchDone chan struct{}
}

func (e *Executor) acquireMergeTurn(ctx context.Context, d Descriptor, r *journal.Record, fence func(context.Context) error) (*mergeTurn, error) {
	route := mergeTurnRoute(d.Repo)
	who := map[string]any{"jobId": d.ID, "epoch": r.Epoch}
	e.addNativeTestGate(d.Repo, who)
	shortWaits := []time.Duration{5 * time.Second, 15 * time.Second}
	var response struct {
		Granted bool            `json:"granted"`
		Reason  string          `json:"reason"`
		Holder  json.RawMessage `json:"holder"`
		Turn    struct {
			Epoch     int64     `json:"epoch"`
			ExpiresAt time.Time `json:"expiresAt"`
		} `json:"turn"`
	}
	for {
		response.Granted, response.Reason, response.Holder = false, "", nil
		if err := fence(ctx); err != nil {
			return nil, err
		}
		if err := e.API.Call(ctx, "POST", route+"/acquire", who, &response); err != nil {
			return nil, err
		}
		if response.Granted {
			break
		}
		if response.Reason != "" {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := fence(ctx); err != nil {
				return nil, err
			}
			message := "マージの順番を取得できません: " + clip(e.redact(response.Reason), mergeErrorLimit)
			return nil, &releaseHold{"review_held", message, map[string]any{"releasePending": true}}
		}
		delay := 30 * time.Second
		if len(shortWaits) > 0 && len(response.Holder) > 0 {
			var holder struct {
				JobID int64 `json:"jobId"`
			}
			if json.Unmarshal(response.Holder, &holder) == nil && holder.JobID > 0 && holder.JobID != d.ID {
				delay, shortWaits = shortWaits[0], shortWaits[1:]
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	who["turnEpoch"] = response.Turn.Epoch
	return e.holdMergeTurn(ctx, route, who, response.Turn.ExpiresAt), nil
}

// mergeTurnRoute is the intake route of a repository's merge turn.
func mergeTurnRoute(repo string) string {
	return "/api/v1/merge-turns/" + url.PathEscape(repo[:indexSlash(repo)]) + "/" + url.PathEscape(repo[indexSlash(repo)+1:])
}

// holdMergeTurn keeps renewal and the confirmed-deadline watchdog independent.
func (e *Executor) holdMergeTurn(ctx context.Context, route string, who map[string]any, expiresAt time.Time) *mergeTurn {
	turnCtx, cancel := context.WithCancel(ctx)
	epoch, _ := who["turnEpoch"].(int64)
	t := &mergeTurn{ctx: turnCtx, cancel: cancel, done: make(chan struct{}), executor: e, route: route, who: who,
		expiresAt: expiresAt, epoch: epoch, slot: make(chan struct{}, 1), changed: make(chan struct{}, 1), watchDone: make(chan struct{})}
	if epoch <= 0 || !expiresAt.After(time.Now()) {
		t.fail()
	}
	go t.watch()
	go func() {
		defer close(t.done)
		for {
			t.mu.Lock()
			interval := min(time.Minute, time.Until(t.expiresAt)/3)
			t.mu.Unlock()
			timer := time.NewTimer(max(time.Nanosecond, interval))
			select {
			case <-turnCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
				if t.renew(turnCtx) != nil {
					return
				}
			}
		}
	}()
	return t
}
func (t *mergeTurn) fail() { t.lost.Store(true); t.cancel() }
func (t *mergeTurn) watch() {
	defer close(t.watchDone)
	for {
		t.mu.Lock()
		left := time.Until(t.expiresAt)
		if left <= 0 || t.epoch <= 0 {
			t.fail()
			t.mu.Unlock()
			return
		}
		t.mu.Unlock()
		timer := time.NewTimer(left)
		select {
		case <-t.ctx.Done():
			timer.Stop()
			return
		case <-t.changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}
func (t *mergeTurn) renew(ctx context.Context) error {
	linked, cancel := context.WithCancel(ctx)
	unlink := context.AfterFunc(t.ctx, cancel)
	defer func() { unlink(); cancel() }()
	select {
	case t.slot <- struct{}{}:
		defer func() { <-t.slot }()
	case <-linked.Done():
		return linked.Err()
	}
	backoff := 100 * time.Millisecond
	for {
		if err := linked.Err(); err != nil {
			return err
		}
		t.mu.Lock()
		expiry := t.expiresAt
		if !time.Now().Before(expiry) {
			t.fail()
		}
		t.mu.Unlock()
		if t.ctx.Err() != nil {
			return t.ctx.Err()
		}
		requestCtx, finish := context.WithDeadline(linked, expiry)
		var out struct {
			Turn struct {
				Epoch     int64     `json:"epoch"`
				ExpiresAt time.Time `json:"expiresAt"`
			} `json:"turn"`
		}
		err := t.executor.API.Call(requestCtx, "POST", t.route+"/renew", t.who, &out)
		if err == nil {
			err = requestCtx.Err()
		}
		finish()
		t.mu.Lock()
		if !time.Now().Before(t.expiresAt) {
			t.fail()
		}
		if t.ctx.Err() != nil {
			t.mu.Unlock()
			return t.ctx.Err()
		}
		if ctx.Err() != nil {
			t.mu.Unlock()
			return ctx.Err()
		}
		if err == nil {
			if out.Turn.Epoch != t.epoch || !out.Turn.ExpiresAt.After(time.Now()) || out.Turn.ExpiresAt.Before(t.expiresAt) {
				err = errors.New("マージ番の更新応答が所有中の番と一致しません")
				t.fail()
			} else {
				t.expiresAt = out.Turn.ExpiresAt
				select {
				case t.changed <- struct{}{}:
				default:
				}
			}
			t.mu.Unlock()
			return err
		}
		var api *intake.APIError
		permanent := intake.TemporaryFailureReason(err) == "" || errors.As(err, &api) && (api.Code == "turn_lost" || api.Code == "lease_lost" || api.Status == 401 || api.Status == 403)
		if permanent {
			t.fail()
		}
		t.mu.Unlock()
		if permanent {
			return err
		}
		timer := time.NewTimer(min(backoff, time.Until(expiry)))
		select {
		case <-linked.Done():
			timer.Stop()
			return linked.Err()
		case <-timer.C:
		}
		backoff = min(2*backoff, time.Second)
	}
}
func (t *mergeTurn) release() bool {
	t.cancel()
	<-t.done
	<-t.watchDone
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = t.executor.API.Call(ctx, "POST", t.route+"/release", t.who, nil)
	return t.lost.Load()
}

// followedTest retains the turn only while the initial test succeeds. A failed
// result is handed to the normal bounded repair path after releasing the turn.
func (e *Executor) followedTest(ctx, parent context.Context, d Descriptor, repo Repository, r *journal.Record, w *gitwork.Workspace, title, prompt string, fence func(context.Context) error, release func() bool) (*intake.State, error) {
	if err := e.checkpoint(ctx, d, r, "test"); err != nil {
		return nil, err
	}
	result, err := e.fixedTest(ctx, repo, r, e.testTargetPreparer(ctx, w, r, title, fence))
	if err != nil {
		return nil, err
	}
	if !result.Passed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if release() {
			return nil, context.Canceled
		}
		ctx = parent
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := fence(ctx); err != nil {
			return nil, err
		}
	}
	return e.implementationTestResult(ctx, d, repo, r, prompt, &result, e.testTargetPreparer(ctx, w, r, title, fence))
}

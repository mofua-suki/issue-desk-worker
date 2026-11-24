package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/providers"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestExecutionErrorReportsSafeViewOnceAndAllowsRedistribution(t *testing.T) {
	now := time.Date(2026, 11, 1, 2, 3, 4, 0, time.UTC)
	for _, response := range []int{204, 503} {
		t.Run(http.StatusText(response), func(t *testing.T) {
			var bodies []map[string]json.RawMessage
			api := leaseClient(t, func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/state") {
					t.Fatalf("unexpected request %s", req.URL.Path)
				}
				data, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(data, []byte("secret")) || bytes.Contains(data, []byte("Bearer")) {
					t.Fatalf("secret in state: %s", data)
				}
				var body map[string]json.RawMessage
				if err := json.Unmarshal(data, &body); err != nil {
					t.Fatal(err)
				}
				bodies = append(bodies, body)
				return leaseResponse(response, "")
			})
			cause := errors.New("secret Bearer credential")
			r := Runner{API: api, Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
				return intake.State{}, cause
			}}, RenewInterval: time.Hour, Now: func() time.Time { return now }}
			for epoch := int64(7); epoch <= 8; epoch++ {
				a := assignment(now)
				a.Lease.Epoch = epoch
				if err := r.execute(context.Background(), a); !errors.Is(err, cause) {
					t.Fatalf("original error lost: %v", err)
				}
				if len(bodies) != int(epoch-6) {
					t.Fatalf("expected one report per execution: %d", len(bodies))
				}
				var status string
				var gotEpoch int64
				var view map[string]json.RawMessage
				body := bodies[len(bodies)-1]
				_ = json.Unmarshal(body["status"], &status)
				_ = json.Unmarshal(body["epoch"], &gotEpoch)
				if err := json.Unmarshal(body["view"], &view); err != nil {
					t.Fatal(err)
				}
				var detail map[string]string
				if err := json.Unmarshal(view["lastExecutionError"], &detail); err != nil {
					t.Fatal(err)
				}
				if status != "running" || gotEpoch != epoch || len(view) != 1 || len(detail) != 2 || detail["kind"] != "execution_error" || detail["at"] != "2026-11-01T02:03:04Z" || body["result"] != nil {
					t.Fatalf("unexpected state: %s", body)
				}
			}
		})
	}
}

type fakeAPI struct {
	renewFn  func(context.Context) (intake.Assignment, error)
	mu       sync.Mutex
	calls    []string
	next     intake.Assignment
	renewErr error
	states   int
	cancel   context.CancelFunc
}

func (f *fakeAPI) add(s string) { f.mu.Lock(); defer f.mu.Unlock(); f.calls = append(f.calls, s) }
func (f *fakeAPI) Config(context.Context) (intake.ConfigSnapshot, error) {
	f.add("config")
	return intake.ConfigSnapshot{}, nil
}
func (f *fakeAPI) Heartbeat(context.Context, intake.HostStatus) error { f.add("heartbeat"); return nil }
func (f *fakeAPI) Next(context.Context, int) (intake.Assignment, error) {
	f.add("next")
	return f.next, nil
}
func (f *fakeAPI) Renew(ctx context.Context, _ int64, _ int64) (intake.Assignment, error) {
	f.add("renew")
	if f.renewFn != nil {
		return f.renewFn(ctx)
	}
	return f.next, f.renewErr
}
func (f *fakeAPI) State(context.Context, int64, int64, intake.State) error {
	f.add("state")
	f.states++
	if f.cancel != nil {
		f.cancel()
	}
	return nil
}

type fakeExecutor struct {
	fn func(context.Context, func(context.Context) error) (intake.State, error)
}

func (fakeExecutor) Ready() bool { return true }
func (f fakeExecutor) Execute(ctx context.Context, _ intake.Job, fence func(context.Context) error) (intake.State, error) {
	return f.fn(ctx, fence)
}
func TestUnimplementedExecutorNeverContactsServer(t *testing.T) {
	f := &fakeAPI{}
	r := Runner{API: f, Executor: UnavailableExecutor{}, Capacity: 1}
	if err := r.Run(context.Background()); !errors.Is(err, ErrExecutorUnavailable) {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatal("未実装なのにサーバー状態を変えました", f.calls)
	}
}
func assignment(now time.Time) intake.Assignment {
	return intake.Assignment{Job: &intake.Job{ID: 2, Repo: "a/b"}, Lease: &intake.Lease{Epoch: 7, ExpiresAt: now.Add(time.Minute)}}
}
func TestLeaseLossPreventsState(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeAPI{next: assignment(now), renewErr: &intake.APIError{Status: 409, Code: "lease_lost"}}
	r := Runner{API: f, Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
		err := fence(ctx)
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Error("lease喪失で実行が止まりません")
		}
		return intake.State{Status: "merged"}, err
	}}, RenewInterval: time.Hour, Now: func() time.Time { return now }}
	if err := r.execute(context.Background(), f.next); err == nil {
		t.Fatal("lease喪失を無視しました")
	}
	if f.states != 0 {
		t.Fatal("lease喪失後に状態を報告しました")
	}
}
func TestSuccessfulExecutionFencesBeforeReporting(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &fakeAPI{next: assignment(now), cancel: cancel}
	r := Runner{API: f, Capacity: 1, Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
		return intake.State{Status: "held", HoldReason: "question"}, nil
	}}, Now: func() time.Time { return now }, RenewInterval: time.Hour, PollInterval: time.Hour, HeartbeatInterval: time.Hour}
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// Completion requests an asynchronous heartbeat; it may run before Run joins
	// its background goroutines. Verify the fence and state order independently.
	want := []string{"config", "heartbeat", "next", "renew", "state"}
	if len(f.calls) != len(want) && (len(f.calls) != len(want)+1 || f.calls[len(want)] != "heartbeat") {
		t.Fatal(f.calls)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Fatal(f.calls)
		}
	}
}
func TestExpiredLeaseNeverRuns(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	a := assignment(now)
	a.Lease.ExpiresAt = now
	r := Runner{API: &fakeAPI{next: a}, Capacity: 1, Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
		t.Fatal("期限切れで実行しました")
		return intake.State{}, nil
	}}, Now: func() time.Time { return now }}
	if err := r.Run(context.Background()); err == nil {
		t.Fatal("期限切れを受理しました")
	}
}

func TestStopCancelsExecutionWithoutFinalState(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	f := &fakeAPI{next: assignment(now)}
	r := Runner{API: f, Executor: fakeExecutor{fn: func(ctx context.Context, _ func(context.Context) error) (intake.State, error) {
		close(started)
		<-ctx.Done()
		return intake.State{}, ctx.Err()
	}}, RenewInterval: time.Hour, Now: func() time.Time { return now }}
	done := make(chan error, 1)
	go func() { done <- r.execute(ctx, f.next) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("停止が実行器へ届きません")
	}
	if f.states != 0 {
		t.Fatal("停止後に最終状態を送信しました")
	}
}

func TestExecutorCannotAbandonRunningJob(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeAPI{next: assignment(now)}
	r := Runner{API: f, Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
		return intake.State{Status: "running"}, nil
	}}, RenewInterval: time.Hour, Now: func() time.Time { return now }}
	if err := r.execute(context.Background(), f.next); err == nil {
		t.Fatal("実行中の仕事を手放しました")
	}
	if f.states != 0 {
		t.Fatal("未完の結果を最終報告しました")
	}
}

func TestCompletedExecutionStopsPeriodicRenewBeforeFinalFence(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	var renewals atomic.Int32
	f := &fakeAPI{next: assignment(now)}
	f.renewFn = func(ctx context.Context) (intake.Assignment, error) {
		if renewals.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return intake.Assignment{}, ctx.Err()
		}
		return f.next, nil
	}
	r := Runner{API: f, Executor: fakeExecutor{fn: func(ctx context.Context, _ func(context.Context) error) (intake.State, error) {
		select {
		case <-started:
			return intake.State{Status: "held", HoldReason: "question"}, nil
		case <-ctx.Done():
			return intake.State{}, ctx.Err()
		}
	}}, RenewInterval: time.Nanosecond, Now: func() time.Time { return now }}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := r.execute(ctx, f.next); err != nil {
		t.Fatal("正常完了時のrenew停止が失敗に変わりました", err)
	}
	if f.states != 1 || renewals.Load() != 2 {
		t.Fatal("定期renew停止後に最後の所有確認と状態報告をしていません", f.states, renewals.Load())
	}
}

func TestExternalStopDuringPeriodicRenewPreventsState(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	started := make(chan struct{})
	f := &fakeAPI{next: assignment(now)}
	f.renewFn = func(ctx context.Context) (intake.Assignment, error) {
		close(started)
		<-ctx.Done()
		return intake.Assignment{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := Runner{API: f, Executor: fakeExecutor{fn: func(ctx context.Context, _ func(context.Context) error) (intake.State, error) {
		select {
		case <-started:
			cancel()
			<-ctx.Done()
			return intake.State{Status: "held"}, nil
		case <-ctx.Done():
			return intake.State{}, ctx.Err()
		}
	}}, RenewInterval: time.Nanosecond, Now: func() time.Time { return now }}
	done := make(chan error, 1)
	go func() { done <- r.execute(ctx, f.next) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("外部停止でrenewが止まりません")
	}
	if f.states != 0 {
		t.Fatal("外部停止を正常終了として報告しました")
	}
}

func TestServerReleasedJobSkipsFinalRenewAndState(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeAPI{next: assignment(now)}
	r := Runner{API: f, Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
		return intake.State{Status: "held", HoldReason: "scope_wait"}, intake.ErrJobReleased
	}}, RenewInterval: time.Hour, Now: func() time.Time { return now }}
	if err := r.execute(context.Background(), f.next); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatal("解放後にrenewまたは最終Stateを送信しました", f.calls)
	}
}

func TestLeaseEventsRecordSafeFailureRecoveryAndExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		defer log.SetOutput(previous)
		var calls atomic.Int32
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return leaseResponse(503, `{"error":"secret-body"}`)
			}
			return leaseResponse(200, renewedJSON(time.Now().Add(2*time.Second)))
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		initial := assignment(time.Now())
		// The recovery response must not move the confirmed deadline backwards.
		initial.Lease.ExpiresAt = time.Now().Add(3 * time.Second)
		guard := newLeaseGuard(Runner{API: api, Now: time.Now}, ctx, cancel, initial)
		if err := guard.renew(ctx); err != nil {
			t.Fatal(err)
		}
		<-ctx.Done()
		<-guard.done
		synctest.Wait() // The watchdog no longer joins log output.
		got := output.String()
		for _, want := range []string{"event=temporary_failure reason=http_5xx", "event=recovered reason=http_5xx", "event=stopped reason=lease_expired", "job=2 epoch=7"} {
			if !strings.Contains(got, want) {
				t.Errorf("missing %q: %s", want, got)
			}
		}
		for _, forbidden := range []string{"secret", "Bearer", "idk_", "https://", "{", "}"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("unsafe log: %s", got)
			}
		}
	})
}

func TestLeaseExpiryDuringFinalReportKeepsExpiryClassification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		initial := assignment(time.Now())
		initial.Lease.ExpiresAt = time.Now().Add(5 * time.Second)
		var states atomic.Int32
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/lease") {
				return leaseResponse(200, renewedJSON(initial.Lease.ExpiresAt))
			}
			states.Add(1)
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		r := Runner{API: api, Now: time.Now, RenewInterval: time.Hour, Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
			return intake.State{Status: "held"}, nil
		}}}
		if err := r.execute(context.Background(), initial); !errors.Is(err, ErrLeaseExpired) {
			t.Fatal(err)
		}
		if states.Load() != 1 || !time.Now().Equal(initial.Lease.ExpiresAt) {
			t.Fatal("report continued after expiry", states.Load(), time.Now())
		}
	})
}

func TestJobErrorKindSafeClassifications(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{ErrLeaseExpired, "lease_expired"},
		{&intake.APIError{Status: 409, Code: "lease_lost"}, "lease_lost"},
		{context.DeadlineExceeded, "timeout"},
		{&intake.APIError{Status: 400}, "api_rejected"},
		{&intake.TransportError{}, "transport_failure"},
		{fmt.Errorf("secret: %w", &gitea.Error{Status: 503}), "gitea_api_error"},
		{fmt.Errorf("secret: %w", &os.PathError{Op: "open", Path: "secret", Err: errors.New("secret")}), "filesystem_error"},
		{fmt.Errorf("secret: %w", &os.LinkError{Op: "link", Old: "secret", New: "secret", Err: errors.New("secret")}), "filesystem_error"},
		{fmt.Errorf("secret: %w", &os.SyscallError{Syscall: "secret", Err: errors.New("secret")}), "filesystem_error"},
		{fmt.Errorf("secret: %w", &net.OpError{Op: "dial", Err: &os.SyscallError{Syscall: "connect", Err: errors.New("secret")}}), "transport_failure"},
		{&providers.ReportOnlyError{Err: errors.New("secret")}, "provider_report_error"},
		{&providers.UsageLimit{Provider: "secret"}, "provider_usage_limit"},
		{errors.New("secret"), "execution_error"},
		{fmt.Errorf("secret: %w", context.Canceled), "canceled"},
	}
	for _, tc := range cases {
		if got := jobErrorKind(tc.err); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

func TestJobErrorKindPriority(t *testing.T) {
	// The order is the published contract. Join each pair to exercise actual
	// overlapping classifications, including UsageLimit before it gains Unwrap.
	cases := []struct {
		err  error
		want string
	}{
		{ErrLeaseExpired, "lease_expired"},
		{&intake.APIError{Status: 409, Code: "lease_lost"}, "lease_lost"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "timeout"},
		{&intake.APIError{Status: 400}, "api_rejected"},
		{&providers.ReportOnlyError{Err: errors.New("report")}, "provider_report_error"},
		{&providers.UsageLimit{}, "provider_usage_limit"},
		{&net.OpError{Op: "dial", Err: errors.New("network")}, "transport_failure"},
		{&gitea.Error{Status: 503}, "gitea_api_error"},
		{&os.PathError{Op: "open", Path: "file", Err: errors.New("filesystem")}, "filesystem_error"},
		{errors.New("execution"), "execution_error"},
	}
	for i, first := range cases {
		if got := jobErrorKind(first.err); got != first.want {
			t.Errorf("example %d: got %s, want %s", i, got, first.want)
		}
		for _, second := range cases[i+1:] {
			for _, joined := range []error{errors.Join(first.err, second.err), errors.Join(second.err, first.err)} {
				if got := jobErrorKind(joined); got != first.want {
					t.Errorf("priority %s over %s in %v: got %s", first.want, second.want, joined, got)
				}
			}
		}
	}
	for _, cause := range []error{
		&os.PathError{Op: "open", Path: "file", Err: errors.New("filesystem")},
		&net.OpError{Op: "dial", Err: errors.New("network")},
		&intake.TransportError{},
	} {
		if got := jobErrorKind(&providers.ReportOnlyError{Err: cause}); got != jobErrorProviderReport {
			t.Errorf("wrapped %T: got %s, want %s", cause, got, jobErrorProviderReport)
		}
		if got := jobErrorKind(errors.Join(cause, &providers.UsageLimit{})); got != jobErrorProviderUsageLimit {
			t.Errorf("usage limit with %T: got %s, want %s", cause, got, jobErrorProviderUsageLimit)
		}
	}
}

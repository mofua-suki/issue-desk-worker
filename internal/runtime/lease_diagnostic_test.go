package runtime

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/intake"
)

type diagnosticExecutor struct {
	job              intake.Job
	reason           string
	at               time.Time
	startedAt        time.Time
	executionStopped bool
}

func (*diagnosticExecutor) Ready() bool { return true }
func (e *diagnosticExecutor) Execute(ctx context.Context, _ intake.Job, fence func(context.Context) error) (intake.State, error) {
	defer func() { e.executionStopped = true }()
	if err := fence(ctx); err != nil {
		return intake.State{}, err
	}
	return intake.State{}, errors.New("unexpected successful renewal")
}
func (e *diagnosticExecutor) RecordLeaseDiagnostic(job intake.Job, reason string, at, startedAt time.Time) error {
	e.job, e.reason, e.at, e.startedAt = job, reason, at, startedAt
	return nil
}

func TestRuntimeRecordsConfirmedExpiryAfterExecutionStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := assignment(time.Now())
		a.Job.Raw = []byte(`{"generation":4}`)
		a.Job.LeaseEpoch = a.Lease.Epoch + 1
		started := time.Now()
		a.Lease.ExpiresAt = time.Now().Add(5 * time.Second)
		api := leaseClient(t, func(*http.Request) (*http.Response, error) { return leaseResponse(503, "") })
		executor := &diagnosticExecutor{}
		r := Runner{API: api, Executor: executor, Now: time.Now, RenewInterval: time.Hour}
		if err := r.execute(context.Background(), a); !errors.Is(err, ErrLeaseExpired) {
			t.Fatal(err)
		}
		if executor.reason != "lease_expired" || executor.job.ID != a.Job.ID || executor.job.LeaseEpoch != a.Lease.Epoch || !executor.at.Equal(a.Lease.ExpiresAt) || !bytes.Equal(executor.job.Raw, a.Job.Raw) || !executor.startedAt.Equal(started) || !executor.executionStopped {
			t.Fatalf("diagnostic: %#v", executor)
		}
	})
}

type finishedExecutor struct{ diagnosticExecutor }

func (*finishedExecutor) Execute(context.Context, intake.Job, func(context.Context) error) (intake.State, error) {
	return intake.State{Status: "merged"}, nil
}

// 最終結果の送信がlease_lostで断られたら、更新の拒否と同じくlease診断を残す。
func TestRuntimeRecordsLeaseLostOnFinalReport(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	a := assignment(now)
	lost := &intake.APIError{Status: 409, Code: "lease_lost"}
	f := &stateRefusingAPI{fakeAPI: fakeAPI{next: a}, err: lost}
	executor := &finishedExecutor{}
	r := Runner{API: f, Executor: executor, Now: func() time.Time { return now }, RenewInterval: time.Hour}
	if err := r.execute(context.Background(), a); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	if executor.reason != "lease_lost" || executor.job.ID != a.Job.ID || executor.job.LeaseEpoch != a.Lease.Epoch {
		t.Fatalf("diagnostic: %#v", executor.diagnosticExecutor)
	}
}

type stateRefusingAPI struct {
	fakeAPI
	err error
}

func (f *stateRefusingAPI) State(context.Context, int64, int64, intake.State) error { return f.err }

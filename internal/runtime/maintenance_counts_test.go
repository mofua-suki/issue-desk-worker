package runtime

import (
	"context"
	"issue-desk-worker/internal/intake"
	"sync/atomic"
	"testing"
	"time"
)

type maintenanceCountsAPI struct {
	fakeAPI
	statuses   chan intake.HostStatus
	job        bool
	dispatched atomic.Bool
}

func (a *maintenanceCountsAPI) Heartbeat(ctx context.Context, s intake.HostStatus) error {
	select {
	case a.statuses <- s:
	case <-ctx.Done():
	}
	return nil
}
func (a *maintenanceCountsAPI) Next(context.Context, int) (intake.Assignment, error) {
	if a.job && !a.dispatched.Swap(true) {
		return assignment(time.Now()), nil
	}
	return intake.Assignment{}, nil
}
func TestMaintenanceCountsOnlyOnceAloneAndBesideJob(t *testing.T) {
	for _, tc := range []struct {
		name              string
		job, publishing   bool
		wantImpl, wantPub int
	}{
		{"手動PRのみ", false, false, 0, 1},
		{"実装ジョブ共存", true, false, 1, 1},
		{"公開ジョブ共存", true, true, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			a := &maintenanceCountsAPI{statuses: make(chan intake.HostStatus, 100), job: tc.job}
			started := make(chan struct{})
			var active atomic.Bool
			e := fakeExecutor{fn: func(ctx context.Context, _ func(context.Context) error) (intake.State, error) {
				active.Store(true)
				close(started)
				<-ctx.Done()
				return intake.State{}, ctx.Err()
			}}
			calls := 0
			r := Runner{API: a, Executor: e, Capacity: 2, RepoLimits: map[string]int{"a/b": 1, "c/d": 1}, PollInterval: time.Millisecond, HeartbeatInterval: time.Millisecond, Status: func() intake.HostStatus {
				n := 0
				if tc.publishing && active.Load() {
					n = 1
				}
				return intake.HostStatus{Publishing: n}
			}, MaintenanceFor: func(ctx context.Context, _ []string) error {
				calls++
				if tc.job && calls == 1 {
					return nil
				}
				if tc.job {
					select {
					case <-started:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				<-ctx.Done()
				return ctx.Err()
			}}
			done := make(chan error, 1)
			go func() { done <- r.Run(ctx) }()
			for {
				select {
				case s := <-a.statuses:
					if s.Publishing != tc.wantPub {
						continue
					}
					if s.Implementing != tc.wantImpl || !s.Busy {
						t.Fatalf("二重計上: %+v", s)
					}
					cancel()
					<-done
					return
				case <-ctx.Done():
					t.Fatal("処理中heartbeat未確認")
				}
			}
		})
	}
}

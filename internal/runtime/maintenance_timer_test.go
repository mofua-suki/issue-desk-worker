package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/intake"
)

func TestRequestWindowsWaitForSuccessfulPollMaintenance(t *testing.T) {
	ctx := context.Background()
	idle, busy, checks, failures := 0, 0, 0, 0
	checkErr := errors.New("host isolation check failed")
	s := &scheduler{ctx: ctx, taskCtx: ctx, byRepo: map[string]int{"a/repo": 1}, r: Runner{
		RepoLimits:         map[string]int{"a/repo": 1, "b/repo": 1},
		PollMaintenanceFor: func(context.Context, []string) error { checks++; return checkErr },
		MaintenanceFor:     func(context.Context, []string) error { idle++; return nil },
		BusyMaintenanceFor: func(context.Context, []string) error { busy++; return nil },
		OnMaintenanceError: func(error) { failures++ },
	}}
	if err := s.runMaintenance(false, true); err != nil {
		t.Fatal(err)
	}
	if checks != 1 || failures != 1 || idle != 0 || busy != 0 {
		t.Fatalf("failed check: checks=%d failures=%d idle=%d busy=%d", checks, failures, idle, busy)
	}
	// Light windows and a full-PC poll must retain the failure without
	// adding heavy checks, including when the host has since been repaired.
	checkErr = nil
	for _, window := range []struct{ full, heavy bool }{{false, false}, {true, false}, {true, true}, {false, false}} {
		if err := s.runMaintenance(window.full, window.heavy); err != nil {
			t.Fatal(err)
		}
		if checks != 1 || idle != 0 || busy != 0 {
			t.Fatalf("blocked window %+v: checks=%d idle=%d busy=%d", window, checks, idle, busy)
		}
	}
	if err := s.runMaintenance(false, true); err != nil {
		t.Fatal(err)
	}
	if checks != 2 || idle != 1 || busy != 1 {
		t.Fatalf("successful recheck: checks=%d idle=%d busy=%d", checks, idle, busy)
	}
	if err := s.runMaintenance(false, false); err != nil {
		t.Fatal(err)
	}
	if checks != 2 || idle != 2 || busy != 2 {
		t.Fatalf("resumed light window: checks=%d idle=%d busy=%d", checks, idle, busy)
	}
}

func TestRequestTimerDoesNotPollJobsOrHeavyMaintenance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		api := &poolAPI{active: map[int64]string{}}
		idle, heavy := 0, 0
		runner := Runner{API: api, Executor: fakeExecutor{}, Capacity: 1, RepoLimits: map[string]int{"a/repo": 1}, PollInterval: time.Hour, HeartbeatInterval: time.Hour, MaintenanceInterval: 15 * time.Second,
			MaintenanceFor:     func(context.Context, []string) error { idle++; return nil },
			PollMaintenanceFor: func(context.Context, []string) error { heavy++; return nil }}
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		synctest.Wait()
		for i := 0; i < 3; i++ {
			time.Sleep(15 * time.Second)
			synctest.Wait()
		}
		if idle != 4 || heavy != 1 || len(api.capacitySeen) != 1 {
			t.Fatalf("requests=%d heavy=%d acquisitions=%d", idle, heavy, len(api.capacitySeen))
		}
		cancel()
		<-done
		time.Sleep(time.Minute)
		synctest.Wait()
		if idle != 4 {
			t.Fatal("requests continued after stop")
		}
	})
}

func TestRequestTimerHonorsBusyRepositories(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		t.Run(string(rune('0'+capacity)), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				api := &poolAPI{queue: []intake.Job{{ID: 1, Repo: "a/repo"}}, active: map[int64]string{}}
				executor := poolExecutor{started: make(chan int64, 1), release: map[int64]chan struct{}{1: make(chan struct{})}}
				idle, busy := [][]string{}, [][]string{}
				runner := Runner{API: api, Executor: executor, Capacity: capacity, RepoLimits: map[string]int{"a/repo": 1, "b/repo": 1}, PollInterval: time.Hour, HeartbeatInterval: time.Hour, RenewInterval: time.Hour, MaintenanceInterval: 15 * time.Second,
					MaintenanceFor:     func(_ context.Context, repos []string) error { idle = append(idle, repos); return nil },
					BusyMaintenanceFor: func(_ context.Context, repos []string) error { busy = append(busy, repos); return nil }}
				done := make(chan error, 1)
				go func() { done <- runner.Run(ctx) }()
				<-executor.started
				synctest.Wait()
				time.Sleep(15 * time.Second)
				synctest.Wait()
				if len(busy) != 1 || len(busy[0]) != 3-capacity {
					t.Fatalf("busy windows: %v", busy)
				}
				if capacity == 1 && len(idle) != 1 || capacity == 2 && (len(idle) != 2 || len(idle[1]) != 1 || idle[1][0] != "b/repo") {
					t.Fatalf("idle windows: %v", idle)
				}
				cancel()
				<-done
			})
		})
	}
}

type requestTimerAPI struct {
	poolAPI
	versionMu sync.Mutex
	version   string
	failures  bool
	calls     atomic.Int64
}

func (a *requestTimerAPI) Config(context.Context) (intake.ConfigSnapshot, error) {
	a.versionMu.Lock()
	defer a.versionMu.Unlock()
	return intake.ConfigSnapshot{ConfigurationVersion: a.version}, nil
}
func (a *requestTimerAPI) NextFor(context.Context, int, []string) (intake.Assignment, error) {
	a.calls.Add(1)
	if a.failures {
		return intake.Assignment{}, &intake.APIError{Status: 503}
	}
	return intake.Assignment{}, nil
}
func TestRequestTimerStopsOnChangedConfigurationAndAcquisitionWait(t *testing.T) {
	for _, kind := range []string{"config", "backoff"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				api := &requestTimerAPI{version: "v1", failures: kind == "backoff"}
				var requests atomic.Int64
				r := Runner{API: api, Executor: fakeExecutor{}, Capacity: 1, RepoLimits: map[string]int{"a/repo": 1}, PollInterval: time.Hour, HeartbeatInterval: time.Hour, MaintenanceInterval: 100 * time.Millisecond, MaintenanceFor: func(context.Context, []string) error { requests.Add(1); return nil }}
				done := make(chan error, 1)
				go func() { done <- r.Run(ctx) }()
				synctest.Wait()
				if kind == "backoff" {
					time.Sleep(900 * time.Millisecond)
					synctest.Wait()
					if requests.Load() != 1 || api.calls.Load() != 1 {
						t.Fatalf("acquisition wait bypassed: requests=%d calls=%d", requests.Load(), api.calls.Load())
					}
					time.Sleep(100 * time.Millisecond)
					synctest.Wait()
					if requests.Load() != 2 || api.calls.Load() != 2 {
						t.Fatalf("retry did not resume: requests=%d calls=%d", requests.Load(), api.calls.Load())
					}
					cancel()
					<-done
				} else {
					api.versionMu.Lock()
					api.version = "v2"
					api.versionMu.Unlock()
					time.Sleep(time.Minute)
					synctest.Wait()
					if err := <-done; err != ErrConfigChanged {
						t.Fatalf("configuration change: %v", err)
					}
					count := requests.Load()
					time.Sleep(time.Minute)
					synctest.Wait()
					if requests.Load() != count || api.calls.Load() != 1 {
						t.Fatal("requests or acquisitions continued after configuration change")
					}
				}
			})
		})
	}
}

type versionedRequestPool struct {
	poolAPI
	versionMu sync.Mutex
	version   string
}

func (a *versionedRequestPool) Config(context.Context) (intake.ConfigSnapshot, error) {
	a.versionMu.Lock()
	defer a.versionMu.Unlock()
	return intake.ConfigSnapshot{ConfigurationVersion: a.version}, nil
}
func TestRequestTimerPausesWhileChangedConfigurationDrainsJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		api := &versionedRequestPool{poolAPI: poolAPI{queue: []intake.Job{{ID: 1, Repo: "a/repo"}}, active: map[int64]string{}}, version: "v1"}
		executor := poolExecutor{started: make(chan int64, 1), release: map[int64]chan struct{}{1: make(chan struct{})}}
		var requests atomic.Int64
		runner := Runner{API: api, Executor: executor, Capacity: 1, RepoLimits: map[string]int{"a/repo": 1}, PollInterval: time.Hour, HeartbeatInterval: time.Hour, RenewInterval: time.Hour, MaintenanceInterval: 15 * time.Second, BusyMaintenanceFor: func(context.Context, []string) error { requests.Add(1); return nil }}
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		<-executor.started
		synctest.Wait()
		api.versionMu.Lock()
		api.version = "v2"
		api.versionMu.Unlock()
		time.Sleep(time.Minute)
		synctest.Wait()
		before := requests.Load()
		if before == 0 {
			t.Fatal("busy requests did not progress before config check")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if requests.Load() != before {
			t.Fatal("busy requests continued while old configuration drained")
		}
		select {
		case err := <-done:
			t.Fatalf("owned job was not drained: %v", err)
		default:
		}
		close(executor.release[1])
		synctest.Wait()
		if err := <-done; err != ErrConfigChanged {
			t.Fatalf("drain: %v", err)
		}
	})
}

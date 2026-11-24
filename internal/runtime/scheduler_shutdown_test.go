package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

func TestSchedulerOrdinaryCancellationWaitsForDurableCleanup(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		t.Run(map[int]string{1: "ordinary", 2: "parent_cancels_fatal_drain"}[capacity], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, err := journal.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				var output bytes.Buffer
				previous := log.Writer()
				log.SetOutput(&output)
				defer log.SetOutput(previous)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var next atomic.Int32
				a := assignment(time.Now())
				a.Lease.ExpiresAt = time.Now().Add(time.Hour)
				api := &shutdownAPI{fakeAPI: &fakeAPI{}, initial: a, next: &next}
				r := Runner{API: api, Capacity: capacity, Now: time.Now, Executor: fakeExecutor{fn: func(ctx context.Context, _ func(context.Context) error) (intake.State, error) {
					<-ctx.Done()
					time.Sleep(6 * time.Second)
					if err := store.Save(&journal.Record{ID: 2, Stage: "cleanup"}); err != nil {
						t.Error(err)
					}
					return intake.State{}, ctx.Err()
				}}}
				done := make(chan error, 1)
				go func() { done <- r.Run(ctx) }()
				synctest.Wait()
				start := time.Now()
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				saved, err := store.Load(2)
				if time.Since(start) != 6*time.Second || err != nil || saved == nil || saved.Stage != "cleanup" {
					t.Fatal("ordinary cancellation abandoned durable cleanup", time.Since(start), saved, err)
				}
				if strings.Contains(output.String(), "event=cancel_deadline") {
					t.Fatal(output.String())
				}
			})
		})
	}
}

func TestSchedulerConfigurationChangeWaitsPastFatalDrainLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var next, states, renew, notReady atomic.Int32
		var changed atomic.Bool
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				version := "c"
				if time.Since(start) >= time.Minute {
					version = "updated"
					changed.Store(true)
				}
				return leaseResponse(200, `{"version":"v","configurationVersion":"`+version+`","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				var status struct {
					Ready bool `json:"ready"`
				}
				if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
					t.Error("heartbeatを読めません", err)
				}
				// 設定の版が変わった後は、取得を止めているのでreadyをfalseで報告する。
				if time.Since(start) >= time.Minute && !status.Ready {
					notReady.Add(1)
				}
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				next.Add(1)
				return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
			case "/api/v1/jobs/2/lease":
				renew.Add(1)
				return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
			case "/api/v1/jobs/2/state":
				states.Add(1)
				return leaseResponse(204, "")
			default:
				t.Error("unexpected route", r.URL.Path)
				return leaseResponse(400, "")
			}
		})
		// 版が変わった後は、古い設定で依頼（仕事の合間・実行中の両方）を処理しない。
		// 前後は、受付が新しい版を返したかで分ける（synctestの時計で動く）。
		var idleBefore, idleAfter, busyBefore, busyAfter atomic.Int32
		count := func(before, after *atomic.Int32) func(context.Context, []string) error {
			return func(context.Context, []string) error {
				if changed.Load() {
					after.Add(1)
				} else {
					before.Add(1)
				}
				return nil
			}
		}
		r := Runner{API: api, Capacity: 1, Now: time.Now, RenewInterval: time.Second, PollInterval: time.Second, MaintenanceFor: count(&idleBefore, &idleAfter), BusyMaintenanceFor: count(&busyBefore, &busyAfter), RepoLimits: map[string]int{"a/b": 1}, Executor: fakeExecutor{fn: func(ctx context.Context, _ func(context.Context) error) (intake.State, error) {
			select {
			case <-ctx.Done():
				t.Error("configuration change canceled execution")
				return intake.State{}, ctx.Err()
			case <-time.After(6 * time.Minute):
			}
			return intake.State{Status: "held", HoldReason: "question"}, nil
		}}}
		err := r.Run(context.Background())
		if err == nil || !errors.Is(err, ErrConfigChanged) || time.Since(start) != 6*time.Minute || states.Load() != 1 || next.Load() != 1 || renew.Load() < 2 {
			t.Fatal("configuration change did not wait for execution/report", err, time.Since(start), states.Load(), next.Load(), renew.Load())
		}
		if notReady.Load() == 0 {
			t.Fatal("設定の版が変わった後もreadyをtrueで報告しました")
		}
		if idleBefore.Load() == 0 || busyBefore.Load() == 0 || idleAfter.Load() != 0 || busyAfter.Load() != 0 {
			t.Fatalf("版が変わった後も依頼を処理しました: 合間 前%d 後%d、実行中 前%d 後%d", idleBefore.Load(), idleAfter.Load(), busyBefore.Load(), busyAfter.Load())
		}
	})
}

func TestSchedulerIdleTemporaryAcquisitionRetriesSafely(t *testing.T) {
	for _, failure := range []string{"503", "connection", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var output bytes.Buffer
				previous := log.Writer()
				log.SetOutput(&output)
				defer log.SetOutput(previous)
				var next atomic.Int32
				api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/api/v1/client-config":
						return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
					case "/api/v1/hosts/fixture":
						return leaseResponse(204, "")
					case "/api/v1/jobs/next":
						next.Add(1)
						switch failure {
						case "connection":
							return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("secret-address")}
						case "timeout":
							return nil, context.DeadlineExceeded
						default:
							return leaseResponse(503, `{"code":"secret-unchecked-code"}`)
						}
					default:
						t.Error("unexpected route", r.URL.Path)
						return leaseResponse(400, "")
					}
				})
				r := Runner{API: api, Capacity: 1, Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
					t.Fatal("execution started without an assignment")
					return intake.State{}, nil
				}}}
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- r.Run(ctx) }()
				time.Sleep(time.Second)
				synctest.Wait()
				cancel()
				err := <-done
				if !errors.Is(err, context.Canceled) || next.Load() != 2 || !strings.Contains(output.String(), "event=acquisition_wait reason=temporary_acquisition_failure") {
					t.Fatal("temporary failure did not retry safely", next.Load(), err, output.String())
				}
				for _, forbidden := range []string{"secret", "request_failed", "acquisition_rejected", "limit_seconds"} {
					if strings.Contains(output.String(), forbidden) {
						t.Fatal("incorrect/unsafe failure detail", output.String())
					}
				}
			})
		})
	}
}

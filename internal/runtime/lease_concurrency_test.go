package runtime

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/intake"
)

func TestPeriodicRenewRecoversWhileExecutionContinues(t *testing.T) {
	for _, failure := range []string{"503", "connection", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				initial := assignment(time.Now())
				var calls atomic.Int32
				recovered := make(chan struct{})
				f := &fakeAPI{next: initial}
				f.renewFn = func(ctx context.Context) (intake.Assignment, error) {
					n := calls.Add(1)
					if n == 1 {
						switch failure {
						case "503":
							return intake.Assignment{}, &intake.APIError{Status: 503}
						case "connection":
							return intake.Assignment{}, &intake.TransportError{}
						case "timeout":
							<-ctx.Done()
							return intake.Assignment{}, ctx.Err()
						}
					}
					if n == 2 {
						close(recovered)
					}
					return assignment(time.Now()), nil
				}
				r := Runner{API: f, Now: time.Now, RenewInterval: time.Second, Executor: fakeExecutor{fn: func(ctx context.Context, _ func(context.Context) error) (intake.State, error) {
					select {
					case <-ctx.Done():
						t.Error("temporary periodic failure canceled execution")
						return intake.State{}, ctx.Err()
					case <-recovered:
						return intake.State{Status: "held"}, nil
					}
				}}}
				if err := r.execute(context.Background(), initial); err != nil {
					t.Fatal(err)
				}
				if calls.Load() != 3 || f.states != 1 {
					t.Fatal(calls.Load(), f.states)
				}
			})
		})
	}
}

func TestPeriodicRenewAndFenceSerializeAndUseLatestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		initial := assignment(time.Now())
		initial.Lease.ExpiresAt = time.Now().Add(10 * time.Second)
		started, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		expiry := time.Now().Add(time.Hour)
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/state") {
				return leaseResponse(204, "")
			}
			checkRenewRequest(t, r)
			n := calls.Add(1)
			if n == 1 {
				close(started)
				<-release
			} else {
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) != 30*time.Second {
					t.Error("queued renewal used stale deadline")
				}
			}
			return leaseResponse(200, renewedJSON(expiry))
		})
		r := Runner{API: api, Now: time.Now, RenewInterval: time.Second, Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
			<-started
			done := make(chan error, 2)
			go func() { done <- fence(ctx) }()
			go func() { done <- fence(ctx) }()
			synctest.Wait()
			if calls.Load() != 1 {
				t.Error("renew requests overlap")
			}
			close(release)
			for range 2 {
				if err := <-done; err != nil {
					return intake.State{}, err
				}
			}
			return intake.State{Status: "held"}, nil
		}}}
		if err := r.execute(context.Background(), initial); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != 4 {
			t.Fatal(calls.Load())
		}
	})
}

func TestLeaseWatchdogCancelsBeforeLateSuccessAndNeverRevives(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		initial := assignment(time.Now())
		initial.Lease.ExpiresAt = time.Now().Add(5 * time.Second)
		started, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		f := &fakeAPI{renewFn: func(context.Context) (intake.Assignment, error) {
			calls.Add(1)
			close(started)
			<-release // deliberately late transport response
			return assignment(time.Now().Add(time.Hour)), nil
		}}
		guard := newLeaseGuard(Runner{API: f, Now: time.Now}, ctx, cancel, initial)
		done := make(chan error, 1)
		go func() { done <- guard.renew(ctx) }()
		<-started
		<-ctx.Done()
		if !time.Now().Equal(initial.Lease.ExpiresAt) {
			t.Fatal("watchdog missed expiry")
		}
		close(release)
		<-done
		if err := guard.renew(context.Background()); !errors.Is(err, ErrLeaseExpired) {
			t.Fatal(err)
		}
		if calls.Load() != 1 {
			t.Fatal("revived after late success")
		}
		<-guard.done
	})
}

func TestFenceWaitingForRenewSlotHonorsCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := make(chan struct{})
		var calls atomic.Int32
		f := &fakeAPI{renewFn: func(request context.Context) (intake.Assignment, error) {
			calls.Add(1)
			close(started)
			<-request.Done()
			return intake.Assignment{}, request.Err()
		}}
		guard := newLeaseGuard(Runner{API: f, Now: time.Now}, ctx, cancel, assignment(time.Now()))
		done := make(chan error, 2)
		go func() { done <- guard.renew(ctx) }()
		<-started
		go func() { done <- guard.renew(ctx) }()
		synctest.Wait()
		cancel()
		for range 2 {
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		}
		if calls.Load() != 1 {
			t.Fatal("queued renewal sent after cancellation")
		}
		<-guard.done
	})
}

func TestSchedulerAcquisitionFailurePreservesLeaseAndDrainsOnRefusal(t *testing.T) {
	for _, failure := range []string{"503", "403"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var next, renew, states atomic.Int32
				initial := assignment(time.Now())
				api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/api/v1/client-config":
						return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
					case "/api/v1/hosts/fixture":
						return leaseResponse(204, "")
					case "/api/v1/jobs/next":
						n := next.Add(1)
						if n == 1 {
							return leaseResponse(200, renewedJSON(initial.Lease.ExpiresAt))
						}
						if failure == "403" || n > 2 {
							return leaseResponse(403, "")
						}
						return leaseResponse(503, "")
					case "/api/v1/jobs/2/lease":
						checkRenewRequest(t, r)
						if renew.Add(1) == 1 {
							return leaseResponse(503, "")
						}
						return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
					case "/api/v1/jobs/2/state":
						states.Add(1)
						return leaseResponse(204, "")
					default:
						t.Error("unexpected route")
						return leaseResponse(400, "")
					}
				})
				r := Runner{API: api, Capacity: 2, Now: time.Now, RenewInterval: time.Hour, PollInterval: time.Second, Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
					if failure == "403" {
						<-ctx.Done()
						return intake.State{}, ctx.Err()
					}
					if err := fence(ctx); err != nil {
						return intake.State{}, err
					}
					return intake.State{Status: "held"}, nil
				}}}
				var refusal *intake.APIError
				err := r.Run(context.Background())
				if !errors.As(err, &refusal) || refusal.Status != 403 {
					t.Fatal(err)
				}
				wantNext := int32(2)
				if failure == "503" {
					wantNext = 3
				}
				if next.Load() != wantNext {
					t.Fatal("unexpected acquisition attempts", next.Load())
				}
				if failure == "503" && states.Load() != 1 || failure == "403" && states.Load() != 0 {
					t.Fatal("scheduler changed unrelated error behavior", states.Load())
				}
			})
		})
	}
}

type leaseExecutor func(context.Context, intake.Job, func(context.Context) error) (intake.State, error)

func (leaseExecutor) Ready() bool { return true }
func (f leaseExecutor) Execute(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
	return f(ctx, job, fence)
}

func TestSchedulerLeaseExpiryDoesNotCancelOtherOwnedJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		var next, states, published atomic.Int32
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				switch next.Add(1) {
				case 1:
					return leaseResponse(200, renewedJSON(start.Add(3*time.Second)))
				case 2:
					return leaseResponse(200, strings.ReplaceAll(renewedJSON(start.Add(time.Minute)), `"id":2`, `"id":3`))
				default:
					return leaseResponse(200, `{"job":null,"lease":null}`)
				}
			case "/api/v1/jobs/2/lease":
				return leaseResponse(503, "")
			case "/api/v1/jobs/3/lease":
				return leaseResponse(200, strings.ReplaceAll(renewedJSON(time.Now().Add(time.Minute)), `"id":2`, `"id":3`))
			case "/api/v1/jobs/3/state":
				states.Add(1)
				cancel()
				return leaseResponse(204, "")
			default:
				t.Error("unexpected route", r.URL.Path)
				return leaseResponse(400, "")
			}
		})
		r := Runner{API: api, Capacity: 2, Now: time.Now, RenewInterval: time.Second, PollInterval: time.Second,
			Executor: leaseExecutor(func(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
				if job.ID == 2 {
					<-ctx.Done()
					if !time.Now().Equal(start.Add(3 * time.Second)) {
						t.Error("expiry did not cancel job at deadline")
					}
					return intake.State{}, ctx.Err()
				}
				time.Sleep(5 * time.Second)
				if ctx.Err() != nil {
					t.Error("another job's expiry canceled execution")
					return intake.State{}, ctx.Err()
				}
				if err := fence(ctx); err != nil {
					return intake.State{}, err
				}
				published.Add(1)
				return intake.State{Status: "held"}, nil
			}),
		}
		if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if states.Load() != 1 || published.Load() != 1 {
			t.Fatal("other job failed to complete", states.Load(), published.Load())
		}
	})
}

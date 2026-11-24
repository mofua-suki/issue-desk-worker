package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/intake"
)

func TestDispatchEventCandidates(t *testing.T) {
	data, err := os.ReadFile("testdata/issue297-dispatch.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Records []struct {
			Common int
			Epoch  int64
			Leased time.Time
			Ended  *time.Time
			Event  string
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil || len(fixture.Records) != 12 {
		t.Fatal("controller event fixture is incomplete", err)
	}
	for _, record := range fixture.Records {
		if record.Ended == nil {
			continue // No duration supplied for the initial/complete-only events.
		}
		for _, candidate := range []string{"long_execution", "transient_timeout", "persistent_transport", "lease_lost", "response_mismatch", "parent_canceled"} {
			t.Run(fmt.Sprintf("%d/epoch%d/%s", record.Common, record.Epoch, candidate), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					parent, cancel := context.WithCancel(context.Background())
					defer cancel()
					start := time.Now()
					initial := assignment(start)
					// 15 minutes is a candidate TTL supplied by the controller,
					// not a recovered ExpiresAt from these dispatch events.
					initial.Lease.ExpiresAt = start.Add(15 * time.Minute)
					confirmed := initial.Lease.ExpiresAt
					var calls, states atomic.Int32
					var reason string
					api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/state") {
							states.Add(1)
							return leaseResponse(204, "")
						}
						checkRenewRequest(t, r)
						if !time.Now().Before(confirmed) {
							t.Error("sent after confirmed deadline")
						}
						n := calls.Add(1)
						switch candidate {
						case "transient_timeout":
							if n == 1 {
								<-r.Context().Done()
								return nil, r.Context().Err()
							}
						case "persistent_transport":
							<-r.Context().Done()
							return nil, r.Context().Err()
						case "lease_lost":
							return leaseResponse(409, `{"code":"lease_lost"}`)
						case "response_mismatch":
							return leaseResponse(200, strings.ReplaceAll(minimalRenewalJSON(confirmed), `"epoch":7`, `"epoch":8`))
						}
						confirmed = time.Now().Add(15 * time.Minute)
						return leaseResponse(200, minimalRenewalJSON(confirmed))
					})
					r := Runner{API: api, Now: time.Now, RenewInterval: time.Minute,
						leaseEvent: func(_, _ int64, event, value string) {
							if event == "stopped" {
								reason = value
							}
						},
						Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
							if candidate == "parent_canceled" {
								time.Sleep(2 * time.Minute)
								cancel()
								<-ctx.Done()
								return intake.State{}, ctx.Err()
							}
							// Replay each observed interval as an external-work wait;
							// actual epoch identities remain in the evidence fixture.
							select {
							case <-ctx.Done():
								return intake.State{}, ctx.Err()
							case <-time.After(record.Ended.Sub(record.Leased) + time.Minute):
							}
							if err := fence(ctx); err != nil {
								return intake.State{}, err
							}
							return intake.State{Status: "held"}, nil
						}},
					}
					err := r.execute(parent, initial)
					synctest.Wait()
					switch candidate {
					case "long_execution", "transient_timeout":
						if err != nil || states.Load() != 1 || calls.Load() < 3 || !confirmed.After(initial.Lease.ExpiresAt) || reason != "" {
							t.Fatal("ordinary wait/transient outage stopped renewal", err, states.Load(), calls.Load(), reason)
						}
					case "persistent_transport":
						if !errors.Is(err, ErrLeaseExpired) || reason != "lease_expired" || states.Load() != 0 || !time.Now().Equal(initial.Lease.ExpiresAt) {
							t.Fatal("persistent outage weakened expiry protection", err, reason, states.Load())
						}
					case "parent_canceled":
						if !errors.Is(err, context.Canceled) || reason != "" || states.Load() != 0 || calls.Load() == 0 {
							t.Fatal("parent stop did not cancel execution/reporting", err, reason, states.Load())
						}
					default:
						if err == nil || reason != candidate || calls.Load() != 1 || states.Load() != 0 {
							t.Fatal("refusal/mismatch did not stop immediately", err, reason, calls.Load(), states.Load())
						}
					}
				})
			})
		}
	}
}

// Models backpressure on the shipped log.Printf -> Writer path. No real PC,
// server, subprocess or logging daemon is required.
type leaseLogGate struct {
	entered chan struct{}
	release chan struct{}
	first   sync.Once
	finish  sync.Once
	match   string
	output  strings.Builder
}

func (w *leaseLogGate) Write(p []byte) (int, error) {
	if w.match != "" && !strings.Contains(string(p), w.match) {
		return len(p), nil
	}
	w.first.Do(func() { close(w.entered) })
	<-w.release
	w.output.Write(p)
	return len(p), nil
}

func (w *leaseLogGate) unblock() { w.finish.Do(func() { close(w.release) }) }

func TestBlockedLeaseLogDoesNotStopRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writer := &leaseLogGate{entered: make(chan struct{}), release: make(chan struct{})}
		previous := log.Writer()
		log.SetOutput(writer)
		defer log.SetOutput(previous)
		defer writer.unblock()
		initial := assignment(time.Now())
		started := time.Now()
		initial.Lease.ExpiresAt = time.Now().Add(10 * time.Second)
		confirmed := initial.Lease.ExpiresAt
		var calls, states atomic.Int32
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/state") {
				states.Add(1)
				return leaseResponse(204, "")
			}
			checkRenewRequest(t, r)
			if !time.Now().Before(confirmed) {
				t.Error("renewal sent after last confirmed deadline")
			}
			if calls.Add(1) == 1 {
				return leaseResponse(503, "")
			}
			confirmed = time.Now().Add(time.Minute)
			return leaseResponse(200, minimalRenewalJSON(confirmed))
		})
		r := Runner{API: api, Now: time.Now, RenewInterval: time.Second,
			Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
				<-writer.entered
				// Cross the original lease deadline while the actual logger is stuck.
				select {
				case <-ctx.Done():
				case <-time.After(20 * time.Second):
				}
				if ctx.Err() != nil || calls.Load() < 2 {
					t.Error("blocked log stopped renewal and expired a running job")
				}
				// Release on the broken path too, so reproduction fails without hanging.
				if ctx.Err() != nil {
					writer.unblock()
					return intake.State{}, ctx.Err()
				}
				if err := fence(ctx); err != nil {
					writer.unblock()
					return intake.State{}, err
				}
				writer.unblock()
				return intake.State{Status: "held"}, nil
			}},
		}
		err := r.execute(context.Background(), initial)
		synctest.Wait()
		if err != nil || states.Load() != 1 {
			t.Fatalf("renewal/publication failed under logging backpressure: %v states=%d", err, states.Load())
		}
		// Queued logs retain observation time, rather than claiming the sink's
		// later recovery time as the request/recovery event time.
		for event, delay := range map[string]time.Duration{"temporary_failure": time.Second, "recovered": 2 * time.Second} {
			want := "event=" + event + " reason=http_5xx at=" + started.Add(delay).UTC().Format(time.RFC3339Nano)
			if !strings.Contains(writer.output.String(), want) {
				t.Error("lost event observation time", want, writer.output.String())
			}
		}
	})
}

func TestLeaseEventQueueIsBoundedAndKeepsStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		var received []string
		events := newLeaseEvents(func(job, epoch int64, event, reason string) {
			if event == "events_dropped" && (job != 0 || epoch != 0) || event != "events_dropped" && (job != 2 || epoch != 7) {
				t.Error("diagnostic identity changed")
			}
			once.Do(func() { close(entered) })
			<-release
			received = append(received, event+":"+reason)
		}, time.Now)
		events.record(2, 7, "temporary_failure", "http_5xx")
		<-entered
		for range 1000 {
			events.record(2, 7, "recovered", "http_5xx")
		}
		events.record(2, 7, "stopped", "lease_expired")
		events.close()
		events.record(2, 7, "ignored", "closed")
		select {
		case <-events.done:
			t.Error("sink unexpectedly returned while blocked")
		default:
		}
		close(release)
		<-events.done
		if len(received) > leaseEventCapacity+2 || received[len(received)-1] != "stopped:lease_expired" {
			t.Fatal("unbounded history or lost terminal event", received)
		}
		if !strings.Contains(strings.Join(received, " "), "events_dropped:buffer_full") {
			t.Fatal("missing history was not disclosed", received)
		}
	})
}

func TestBlockedRecoveryLogDoesNotDelayWatchdog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writer := &leaseLogGate{entered: make(chan struct{}), release: make(chan struct{}), match: "event=recovered"}
		previous := log.Writer()
		log.SetOutput(writer)
		defer log.SetOutput(previous)
		defer writer.unblock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		initial := assignment(time.Now())
		initial.Lease.ExpiresAt = time.Now().Add(10 * time.Second)
		var calls atomic.Int32
		var confirmed time.Time
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			checkRenewRequest(t, r)
			if calls.Add(1) == 1 {
				return leaseResponse(503, "")
			}
			confirmed = time.Now().Add(20 * time.Second)
			return leaseResponse(200, minimalRenewalJSON(confirmed))
		})
		guard := newLeaseGuard(Runner{API: api, Now: time.Now}, ctx, cancel, initial)
		if err := guard.renew(ctx); err != nil {
			t.Fatal(err)
		}
		<-writer.entered
		<-ctx.Done()
		<-guard.done
		if !time.Now().Equal(confirmed) {
			t.Error("blocked recovery log delayed the watchdog")
		}
		if _, err := guard.current(); !errors.Is(err, ErrLeaseExpired) {
			t.Error("expiry was not sticky", err)
		}
		if err := guard.renew(context.Background()); !errors.Is(err, ErrLeaseExpired) || calls.Load() != 2 {
			t.Error("renewed after loss", err, calls.Load())
		}
		writer.unblock()
		<-guard.events.done
	})
}

func TestSchedulerRenewsLongJobsWhileLogWriterIsBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		writer := &leaseLogGate{entered: make(chan struct{}), release: make(chan struct{})}
		previous := log.Writer()
		log.SetOutput(writer)
		defer log.SetOutput(previous)
		defer writer.unblock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		var mu sync.Mutex
		confirmed := [2]time.Time{start.Add(15 * time.Minute), start.Add(15 * time.Minute)}
		var calls [2]int
		var next, states atomic.Int32
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				switch next.Add(1) {
				case 1:
					return leaseResponse(200, renewedJSON(confirmed[0]))
				case 2:
					return leaseResponse(200, strings.ReplaceAll(renewedJSON(confirmed[1]), `"id":2`, `"id":3`))
				default:
					return leaseResponse(200, `{"job":null,"lease":null}`)
				}
			case "/api/v1/jobs/2/state", "/api/v1/jobs/3/state":
				if states.Add(1) == 2 {
					cancel()
				}
				return leaseResponse(204, "")
			case "/api/v1/jobs/2/lease", "/api/v1/jobs/3/lease":
				index := 0
				if strings.Contains(r.URL.Path, "/3/") {
					index = 1
				}
				mu.Lock()
				defer mu.Unlock()
				if !time.Now().Before(confirmed[index]) {
					t.Error("renewal reached intake after confirmed deadline")
				}
				calls[index]++
				if index == 0 && calls[index] == 1 {
					return leaseResponse(503, "")
				}
				confirmed[index] = time.Now().Add(15 * time.Minute)
				body := minimalRenewalJSON(confirmed[index])
				if index == 1 {
					body = strings.ReplaceAll(body, `"id":2`, `"id":3`)
				}
				return leaseResponse(200, body)
			default:
				t.Error("unexpected route", r.URL.Path)
				return leaseResponse(400, "")
			}
		})
		// Exercise the shipped Run path with its default one-minute renewals and
		// a single shared diagnostic consumer, across more than two lease periods.
		r := Runner{API: api, Capacity: 2, Executor: leaseExecutor(func(ctx context.Context, _ intake.Job, fence func(context.Context) error) (intake.State, error) {
			<-writer.entered
			select {
			case <-ctx.Done():
				writer.unblock()
				cancel()
				t.Error("logging canceled a long-running job")
				return intake.State{}, ctx.Err()
			case <-time.After(35 * time.Minute):
			}
			if err := fence(ctx); err != nil {
				writer.unblock()
				cancel()
				return intake.State{}, err
			}
			return intake.State{Status: "held"}, nil
		})}
		err := r.Run(ctx)
		writer.unblock()
		synctest.Wait()
		if !errors.Is(err, context.Canceled) || states.Load() != 2 || calls[0] < 30 || calls[1] < 30 {
			t.Fatal("long jobs did not renew and report independently", err, states.Load(), calls)
		}
	})
}

func TestLeaseEventAfterCloseRecordsMissingHistory(t *testing.T) {
	events := newLeaseEvents(func(_, _ int64, _, _ string) {
		t.Error("closed queue must not send a new event to its sink")
	}, time.Now)
	events.close()
	<-events.done
	events.record(2, 7, "stopped", "lease_expired")
	if !events.closedDropped.Load() {
		t.Fatal("event after consumer exit disappeared without missing-history evidence")
	}
}

func TestLeaseEventUsesRunnerClock(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)
	at := time.Date(2026, 11, 7, 9, 0, 0, 123, time.FixedZone("fixture", 9*60*60))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newLeaseGuard(Runner{Now: func() time.Time { return at }, API: &fakeAPI{
		renewErr: &intake.APIError{Status: 409, Code: "lease_lost"},
	}}, ctx, cancel, assignment(at))
	if err := guard.renew(ctx); err == nil {
		t.Fatal("missing refusal")
	}
	<-guard.done
	<-guard.events.done
	if !strings.Contains(output.String(), "event=stopped reason=lease_lost at="+at.UTC().Format(time.RFC3339Nano)) {
		t.Fatal("event timestamp does not use the lease clock", output.String())
	}
}

func TestSchedulerLateCleanupKeepsLeaseEventQueueOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var next atomic.Int32
		initial := assignment(start)
		initial.Lease.ExpiresAt = start.Add(shutdownDrainLimit + shutdownCancelLimit + time.Second)
		api := &shutdownAPI{fakeAPI: &fakeAPI{}, initial: initial, next: &next}
		var stopped atomic.Int32
		finished := make(chan struct{})
		r := Runner{API: api, Capacity: 2, Now: time.Now,
			leaseEvent: func(job, epoch int64, event, reason string) {
				if event == "stopped" && reason == "lease_expired" && job == 2 && epoch == 7 {
					stopped.Add(1)
				}
			},
			Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
				defer close(finished)
				<-ctx.Done()
				time.Sleep(shutdownCancelLimit + time.Second)
				// A guard check during late cleanup observes expiry after Run
				// returned; it must still be able to record its stopped event.
				if err := fence(context.Background()); !errors.Is(err, ErrLeaseExpired) {
					t.Error("late ownership check did not observe expiry", err)
				}
				return intake.State{}, ctx.Err()
			}},
		}
		if err := r.Run(context.Background()); err == nil {
			t.Fatal("missing acquisition refusal")
		}
		if time.Since(start) != shutdownDrainLimit+shutdownCancelLimit {
			t.Fatal("changed the cancellation join bound", time.Since(start))
		}
		if stopped.Load() != 0 {
			t.Fatal("fixture did not defer the event past Run return")
		}
		<-finished
		synctest.Wait()
		if stopped.Load() != 1 {
			t.Fatal("late cleanup event disappeared after Run returned", stopped.Load())
		}
	})
}

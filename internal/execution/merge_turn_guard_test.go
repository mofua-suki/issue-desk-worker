package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/journal"
)

type turnTransport func(*http.Request) (*http.Response, error)

func (f turnTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func turnTestAPI(t *testing.T, handler http.HandlerFunc) *intake.Client {
	t.Helper()
	api, err := intake.New(configBootstrap("https://intake.example"), "idk_"+strings.Repeat("a", 43), turnTransport(func(r *http.Request) (*http.Response, error) {
		w := &turnResponse{header: make(http.Header)}
		handler(w, r)
		return &http.Response{StatusCode: w.status, Header: w.header, Body: io.NopCloser(strings.NewReader(w.body.String())), Request: r}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	return api
}

type turnResponse struct {
	header http.Header
	status int
	body   strings.Builder
}

func (w *turnResponse) Header() http.Header    { return w.header }
func (w *turnResponse) WriteHeader(status int) { w.status = status }
func (w *turnResponse) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.body.Write(b)
}

func TestAcquireMergeTurnPolling(t *testing.T) {
	const (
		occupied = `{"granted":false,"holder":{"jobId":2}}`
		denied   = `{"granted":false}`
		granted  = `{"granted":true}`
	)
	for _, tc := range []struct {
		name      string
		responses []string
		atSeconds []int
		cancelAt  time.Duration
	}{
		{"early release", []string{occupied, granted}, []int{0, 5}, 0},
		{"long occupation", []string{occupied, occupied, occupied, occupied, granted}, []int{0, 5, 20, 50, 80}, 0},
		{"holder disappears", []string{occupied, denied, occupied, granted}, []int{0, 5, 35, 50}, 0},
		{"invalid holder", []string{
			`{"holder":null}`, `{"holder":{}}`, `{"holder":{"jobId":-2}}`,
			`{"holder":{"jobId":"2"}}`, `{"holder":{"jobId":1}}`, occupied, granted,
		}, []int{0, 30, 60, 90, 120, 150, 155}, 0},
		{"canceled while waiting", []string{occupied}, []int{0}, 2 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if tc.cancelAt > 0 {
					timer := time.AfterFunc(tc.cancelAt, cancel)
					defer timer.Stop()
				}
				calls := 0
				api := turnTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/merge-turns/owner/project/release":
						fmt.Fprint(w, `{}`)
					case "/api/v1/merge-turns/owner/project/acquire":
						if calls >= len(tc.responses) {
							t.Fatalf("unexpected acquire at %v", time.Since(start))
						}
						if want := time.Duration(tc.atSeconds[calls]) * time.Second; time.Since(start) != want {
							t.Fatalf("acquire %d at %v, want %v", calls+1, time.Since(start), want)
						}
						response := tc.responses[calls]
						calls++
						if response == granted {
							fmt.Fprintf(w, `{"granted":true,"turn":{"epoch":7,"expiresAt":%q}}`, time.Now().Add(time.Minute).Format(time.RFC3339Nano))
						} else {
							fmt.Fprint(w, response)
						}
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				})
				e := &Executor{API: api}
				d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Repo: "owner/project"}}
				turn, err := e.acquireMergeTurn(ctx, d, &journal.Record{Epoch: 2}, func(context.Context) error { return nil })
				if tc.cancelAt > 0 {
					if !errors.Is(err, context.Canceled) || turn != nil || time.Since(start) != tc.cancelAt {
						t.Fatalf("after %v got turn=%v, err=%v", time.Since(start), turn, err)
					}
				} else {
					if err != nil || turn == nil {
						t.Fatalf("acquire returned turn=%v, err=%v", turn, err)
					}
					if turn.release() {
						t.Error("granted turn was lost")
					}
				}
				if calls != len(tc.responses) {
					t.Fatalf("got %d acquire requests, want %d", calls, len(tc.responses))
				}
			})
		})
	}
}

func TestTurnRenewalProtectsConfirmedDeadline(t *testing.T) {
	for _, kind := range []string{"recover", "turn_lost", "lease_lost", "unauthorized", "forbidden", "contract", "missing", "epoch", "old", "expired", "outage", "blocked"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				expiry := time.Now().Add(800 * time.Millisecond)
				entered := make(chan struct{}, 1)
				api := turnTestAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/release") {
						fmt.Fprint(w, `{}`)
						return
					}
					n := calls.Add(1)
					select {
					case entered <- struct{}{}:
					default:
					}
					switch kind {
					case "recover":
						if n == 2 {
							w.WriteHeader(503)
							fmt.Fprint(w, `{}`)
							return
						}
					case "turn_lost", "lease_lost":
						w.WriteHeader(409)
						fmt.Fprintf(w, `{"code":%q}`, kind)
						return
					case "unauthorized":
						w.WriteHeader(401)
						return
					case "forbidden":
						w.WriteHeader(403)
						return
					case "contract":
						w.WriteHeader(503)
						fmt.Fprint(w, `{"code":"contract_mismatch"}`)
						return
					case "missing":
						fmt.Fprint(w, `{}`)
						return
					case "epoch":
						fmt.Fprintf(w, `{"turn":{"epoch":8,"expiresAt":%q}}`, expiry.Format(time.RFC3339Nano))
						return
					case "old", "expired":
						deadline := expiry.Add(-time.Millisecond)
						if kind == "expired" {
							deadline = time.Now().Add(-time.Second)
						}
						fmt.Fprintf(w, `{"turn":{"epoch":7,"expiresAt":%q}}`, deadline.Format(time.RFC3339Nano))
						return
					case "outage":
						w.WriteHeader(503)
						return
					case "blocked":
						<-r.Context().Done()
						return
					}
					fmt.Fprintf(w, `{"turn":{"epoch":7,"expiresAt":%q}}`, expiry.Format(time.RFC3339Nano))
				}))
				turn := (&Executor{API: api}).holdMergeTurn(context.Background(), mergeTurnRoute("owner/project"), map[string]any{"turnEpoch": int64(7)}, expiry)
				defer turn.release()
				if kind == "recover" {
					if err := turn.renew(context.Background()); err != nil {
						t.Fatal(err)
					}
					if err := turn.renew(context.Background()); err != nil {
						t.Fatal(err)
					}
					if calls.Load() != 3 || turn.ctx.Err() != nil {
						t.Fatalf("calls=%d ctx=%v", calls.Load(), turn.ctx.Err())
					}
					return
				}
				done := make(chan error, 1)
				go func() { done <- turn.renew(context.Background()) }()
				select {
				case <-turn.ctx.Done():
				case <-time.After(2 * time.Second):
					t.Fatal("turn did not stop")
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("renew accepted invalid ownership")
					}
				case <-time.After(time.Second):
					t.Fatal("renew did not cancel")
				}
				if !turn.lost.Load() {
					t.Fatal("ownership loss not recorded")
				}
				if kind != "outage" && kind != "blocked" && calls.Load() != 1 {
					t.Fatalf("rejected renewal retried: %d", calls.Load())
				}
			})
		})
	}
}

func TestTurnReleaseCancelsConcurrentRenewals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		var calls atomic.Int32
		var released atomic.Bool
		api := turnTestAPI(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/release") {
				released.Store(true)
				fmt.Fprint(w, `{}`)
				return
			}
			if calls.Add(1) == 1 {
				close(entered)
			}
			<-r.Context().Done()
		}))
		turn := (&Executor{API: api}).holdMergeTurn(context.Background(), mergeTurnRoute("owner/project"), map[string]any{"turnEpoch": int64(7)}, time.Now().Add(600*time.Millisecond))
		done := make(chan error, 2)
		go func() { done <- turn.renew(context.Background()) }()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("renew not entered")
		}
		go func() { done <- turn.renew(context.Background()) }()
		// The periodic renewal also competes for the same slot while the first blocks.
		select {
		case <-turn.ctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("watchdog blocked by renewal")
		}
		turn.release()
		for i := 0; i < 2; i++ {
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("renew succeeded after expiry")
				}
			case <-time.After(time.Second):
				t.Fatal("waiting renewal blocked")
			}
		}
		if calls.Load() != 1 || !released.Load() {
			t.Fatalf("calls=%d release=%v", calls.Load(), released.Load())
		}
	})
}

func TestTurnRenewalExtendsOnlyConfirmedExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		original := time.Now().Add(3 * time.Second)
		calls := 0
		api := turnTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/release") {
				fmt.Fprint(w, `{}`)
				return
			}
			calls++
			if calls == 1 {
				fmt.Fprintf(w, `{"turn":{"epoch":7,"expiresAt":%q}}`, original.Add(3*time.Second).Format(time.RFC3339Nano))
				return
			}
			<-r.Context().Done()
		})
		turn := (&Executor{API: api}).holdMergeTurn(context.Background(), mergeTurnRoute("owner/project"), map[string]any{"turnEpoch": int64(7)}, original)
		defer turn.release()
		if err := turn.renew(context.Background()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if turn.ctx.Err() != nil {
			t.Fatal("validated renewal did not extend deadline")
		}
		time.Sleep(3 * time.Second)
		synctest.Wait()
		if turn.ctx.Err() == nil || !turn.lost.Load() {
			t.Fatal("blocked renewal extended confirmed deadline")
		}
	})
}

func TestTurnParentCancellationAndReleaseInterruptRenewal(t *testing.T) {
	for _, kind := range []string{"parent", "release"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				entered := make(chan struct{})
				api := turnTestAPI(t, func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/release") {
						fmt.Fprint(w, `{}`)
						return
					}
					close(entered)
					<-r.Context().Done()
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				turn := (&Executor{API: api}).holdMergeTurn(ctx, mergeTurnRoute("owner/project"), map[string]any{"turnEpoch": int64(7)}, time.Now().Add(time.Hour))
				done := make(chan error, 1)
				go func() { done <- turn.renew(context.Background()) }()
				<-entered
				if kind == "parent" {
					cancel()
				}
				if turn.release() {
					t.Fatal("normal cancellation marked ownership lost")
				}
				if err := <-done; err == nil {
					t.Fatal("renewal survived cancellation")
				}
			})
		})
	}
}

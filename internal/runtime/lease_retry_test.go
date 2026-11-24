package runtime

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/intake"
)

type leaseTransport func(*http.Request) (*http.Response, error)

func (f leaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func leaseResponse(status int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

// The shipped Client sends actual contract JSON through a fake transport. The
// fixture models the reported server contract: epoch-only renewals, no progress
// mutation, and safe renewal after the server committed an unobserved response.
func leaseClient(t *testing.T, transport leaseTransport) *intake.Client {
	t.Helper()
	c, err := intake.New(config.Bootstrap{ServerURL: "https://intake.invalid", ServerTokenFile: "fixture", WorkerID: "fixture"}, "idk_"+strings.Repeat("a", 43), transport)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func checkRenewRequest(t *testing.T, r *http.Request) {
	t.Helper()
	var body map[string]json.RawMessage
	if r.Method != "POST" || r.URL.Path != "/api/v1/jobs/2/lease" || json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || string(body["epoch"]) != "7" {
		t.Error("renew must preserve job/epoch and omit progress")
	}
}

func renewedJSON(expiry time.Time) string {
	data, _ := json.Marshal(assignment(expiry.Add(-time.Minute)))
	return string(data)
}

// Renew can return just the ownership fields rather than Next's descriptor.
// This is a compatibility fixture, not a claim about the deployed server shape.
func minimalRenewalJSON(expiry time.Time) string {
	data, _ := json.Marshal(map[string]any{
		"job":   map[string]any{"id": 2},
		"lease": intake.Lease{Epoch: 7, ExpiresAt: expiry},
	})
	return string(data)
}

func TestRenewTransientRecoveryThroughClientAndFence(t *testing.T) {
	for _, failure := range []string{"503", "connection", "timeout", "lost_response"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls, states, published atomic.Int32
				var events []string
				initial := assignment(time.Now())
				serverExpiry := initial.Lease.ExpiresAt
				api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/state") {
						states.Add(1)
						return leaseResponse(204, "")
					}
					checkRenewRequest(t, r)
					if calls.Add(1) == 1 {
						switch failure {
						case "503":
							return leaseResponse(503, `{"error":"secret-response"}`)
						case "connection":
							return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("secret-address")}
						case "timeout":
							<-r.Context().Done()
							return nil, r.Context().Err()
						case "lost_response":
							serverExpiry = time.Now().Add(10 * time.Minute)
							return nil, io.EOF
						}
					}
					serverExpiry = serverExpiry.Add(time.Minute)
					return leaseResponse(200, minimalRenewalJSON(serverExpiry))
				})
				r := Runner{API: api, Now: time.Now, RenewInterval: time.Hour,
					leaseEvent: func(_, _ int64, event, reason string) { events = append(events, event+":"+reason) },
					Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
						complete := make(chan error, 1)
						go func() { complete <- fence(ctx) }()
						synctest.Wait()
						if ctx.Err() != nil || published.Load() != 0 {
							t.Error("temporary failure canceled execution or published")
						}
						select {
						case <-complete:
							t.Fatal("fence passed during failure")
						default:
						}
						if err := <-complete; err != nil {
							return intake.State{}, err
						}
						published.Add(1)
						return intake.State{Status: "held"}, nil
					}},
				}
				if err := r.execute(context.Background(), initial); err != nil {
					t.Fatal(err)
				}
				synctest.Wait() // Wait for the asynchronous diagnostic sink before inspecting it.
				if calls.Load() < 3 || states.Load() != 1 || published.Load() != 1 {
					t.Fatal(calls.Load(), states.Load(), published.Load())
				}
				wantReason := map[string]string{"503": "http_5xx", "connection": "connection_failed", "timeout": "request_timeout", "lost_response": "connection_failed"}[failure]
				if len(events) != 2 || events[0] != "temporary_failure:"+wantReason || events[1] != "recovered:"+wantReason {
					t.Fatal(events)
				}
			})
		})
	}
}

func TestRenewAcceptsOptionalDescriptorFields(t *testing.T) {
	for _, shape := range []string{"omitted", "empty_repo", "repo_only", "repo_case", "job_epoch_only", "full"} {
		t.Run(shape, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				initial := assignment(time.Now())
				var renewals, published, states atomic.Int32
				api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/state") {
						states.Add(1)
						return leaseResponse(204, "")
					}
					checkRenewRequest(t, r)
					renewals.Add(1)
					job := map[string]any{"id": 2}
					switch shape {
					case "empty_repo":
						job["repo"] = ""
					case "repo_case":
						job["repo"] = "A/B"
					case "repo_only", "full":
						job["repo"] = "a/b"
					}
					if shape == "job_epoch_only" || shape == "full" {
						job["leaseEpoch"] = 7
					}
					data, _ := json.Marshal(map[string]any{"job": job, "lease": initial.Lease})
					return leaseResponse(200, string(data))
				})
				r := Runner{API: api, Now: time.Now, RenewInterval: time.Hour, Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
					if err := fence(ctx); err != nil {
						return intake.State{}, err
					}
					published.Add(1)
					return intake.State{Status: "held"}, nil
				}}}
				if err := r.execute(context.Background(), initial); err != nil {
					t.Fatal("compatible renewal rejected", err)
				}
				if renewals.Load() != 2 || published.Load() != 1 || states.Load() != 1 {
					t.Fatal("fence and final report did not complete", renewals.Load(), published.Load(), states.Load())
				}
				// The acquisition contract still requires the repository.
				if (intake.Assignment{Job: &intake.Job{ID: 2}, Lease: initial.Lease}).Validate(time.Now()) == nil {
					t.Fatal("Next descriptor validation was weakened")
				}
			})
		})
	}
}

func TestRenewPersistentFailureStopsExactlyAtConfirmedExpiry(t *testing.T) {
	for _, failure := range []string{"503", "request_wait", "lost_response"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				initial := assignment(time.Now())
				initial.Lease.ExpiresAt = time.Now().Add(5 * time.Second)
				var calls, states atomic.Int32
				api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/state") {
						states.Add(1)
						return leaseResponse(204, "")
					}
					checkRenewRequest(t, r)
					calls.Add(1)
					if failure == "request_wait" {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					if failure == "lost_response" {
						return nil, io.EOF
					} // server success is deliberately unknown
					return leaseResponse(503, "secret-response")
				})
				stopped := time.Time{}
				r := Runner{API: api, Now: time.Now, RenewInterval: time.Hour, Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
					if err := fence(ctx); err == nil {
						t.Error("expired fence passed")
					}
					<-ctx.Done()
					stopped = time.Now()
					// A second fence can never revive a lost lease.
					before := calls.Load()
					_ = fence(context.Background())
					if calls.Load() != before {
						t.Error("renewed after loss")
					}
					return intake.State{Status: "merged"}, nil
				}}}
				if err := r.execute(context.Background(), initial); !errors.Is(err, ErrLeaseExpired) {
					t.Fatal(err)
				}
				if !stopped.Equal(initial.Lease.ExpiresAt) || states.Load() != 0 {
					t.Fatal(stopped, states.Load())
				}
			})
		})
	}
}

func TestRenewRejectsPermanentErrorsAndMismatchedResponses(t *testing.T) {
	for _, failure := range []string{"lease_lost", "epoch", "job_epoch", "job", "repo", "nil_job", "nil_lease", "expiry_boundary", "rollback", "401", "403", "contract", "invalid_json", "redirect", "certificate"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				initial := assignment(time.Now())
				var calls atomic.Int32
				api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					checkRenewRequest(t, r)
					out := assignment(time.Now())
					switch failure {
					case "lease_lost":
						return leaseResponse(409, `{"code":"lease_lost"}`)
					case "401":
						return leaseResponse(401, "secret")
					case "403":
						return leaseResponse(403, "secret")
					case "contract":
						return leaseResponse(503, `{"code":"contract_mismatch"}`)
					case "invalid_json":
						return leaseResponse(200, "secret invalid JSON")
					case "redirect":
						return leaseResponse(307, "")
					case "certificate":
						return nil, &tls.CertificateVerificationError{Err: errors.New("secret-certificate")}
					case "epoch":
						out.Lease.Epoch++
					case "job_epoch":
						out.Job.LeaseEpoch = 8
					case "job":
						out.Job.ID++
					case "repo":
						out.Job.Repo = "different/repo"
					case "nil_job":
						out.Job = nil
					case "nil_lease":
						out.Lease = nil
					case "expiry_boundary":
						out.Lease.ExpiresAt = time.Now()
					case "rollback":
						out.Lease.ExpiresAt = initial.Lease.ExpiresAt.Add(-time.Second)
					}
					data, _ := json.Marshal(out)
					return leaseResponse(200, string(data))
				})
				r := Runner{API: api, Now: time.Now, RenewInterval: time.Hour, Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
					err := fence(ctx)
					if err == nil || ctx.Err() == nil {
						t.Error("invalid ownership did not immediately cancel")
					}
					return intake.State{Status: "merged"}, err
				}}}
				start := time.Now()
				if err := r.execute(context.Background(), initial); err == nil || strings.Contains(err.Error(), "secret") {
					t.Fatal(err)
				}
				if calls.Load() != 1 || time.Now() != start {
					t.Fatal("permanent failure retried", calls.Load())
				}
			})
		})
	}
}

func TestRenewParentCancellationDuringRequestAndBackoff(t *testing.T) {
	for _, waiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "backoff", true: "request"}[waiting], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var calls atomic.Int32
				api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
					calls.Add(1)
					if waiting {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return leaseResponse(503, "")
				})
				r := Runner{API: api, Now: time.Now, RenewInterval: time.Hour, Executor: fakeExecutor{fn: func(jobCtx context.Context, fence func(context.Context) error) (intake.State, error) {
					done := make(chan error, 1)
					go func() { done <- fence(jobCtx) }()
					synctest.Wait()
					cancel()
					return intake.State{}, <-done
				}}}
				if err := r.execute(ctx, assignment(time.Now())); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if calls.Load() != 1 {
					t.Fatal("request after parent cancellation", calls.Load())
				}
			})
		})
	}
}

type failedResponseBody struct{}

func (failedResponseBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failedResponseBody) Close() error             { return nil }

func TestRenewRefusalWithUnreadableBodyIsNotRetryable(t *testing.T) {
	for _, status := range []int{401, 403, 409} {
		synctest.Test(t, func(t *testing.T) {
			var calls atomic.Int32
			api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: failedResponseBody{}}, nil
			})
			r := Runner{API: api, Now: time.Now, RenewInterval: time.Hour, Executor: fakeExecutor{fn: func(ctx context.Context, fence func(context.Context) error) (intake.State, error) {
				return intake.State{}, fence(ctx)
			}}}
			var refusal *intake.APIError
			err := r.execute(context.Background(), assignment(time.Now()))
			if !errors.As(err, &refusal) || refusal.Status != status || intake.TemporaryFailureReason(err) != "" || calls.Load() != 1 {
				t.Fatal(err, calls.Load())
			}
		})
	}
}

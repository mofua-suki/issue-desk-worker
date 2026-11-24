package runtime

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/execution"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// Use the shipped Executor's descriptor validation and Reported implementation.
// The peer and transport failures are controlled without real jobs or API calls.
type failingPipeline struct {
	*execution.Executor
	run leaseExecutor
}

func (f failingPipeline) Ready() bool { return true }
func (f failingPipeline) Execute(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
	return f.run(ctx, job, fence)
}

func TestSchedulerJobErrorsDoNotCancelPeerReporting(t *testing.T) {
	for _, mode := range []string{"descriptor", "execution", "report_refused", "report_unavailable", "report_contract"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var logs bytes.Buffer
				previous := log.Writer()
				log.SetOutput(&logs)
				defer log.SetOutput(previous)
				store, err := journal.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var next, peerSaved, errorReports atomic.Int32
				api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/api/v1/client-config":
						return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
					case "/api/v1/hosts/fixture":
						return leaseResponse(204, "")
					case "/api/v1/jobs/next":
						switch next.Add(1) {
						case 1:
							return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
						case 2:
							return leaseResponse(200, strings.ReplaceAll(renewedJSON(time.Now().Add(time.Minute)), `"id":2`, `"id":3`))
						default:
							return leaseResponse(200, `{"job":null,"lease":null}`)
						}
					case "/api/v1/jobs/2/lease":
						return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
					case "/api/v1/jobs/3/lease":
						return leaseResponse(200, strings.ReplaceAll(renewedJSON(time.Now().Add(time.Minute)), `"id":2`, `"id":3`))
					case "/api/v1/jobs/2/state":
						errorReports.Add(1)
						if mode == "report_refused" {
							return leaseResponse(400, `{"code":"secret-error"}`)
						}
						if mode == "report_contract" {
							// 契約の食い違いは送り直しても通らないので、1回で終える。
							return leaseResponse(500, `{"code":"contract_mismatch"}`)
						}
						return leaseResponse(503, "")
					case "/api/v1/jobs/3/state":
						if r.Context().Err() != nil {
							t.Error("peer report canceled")
						}
						peerSaved.Add(1)
						cancel()
						return leaseResponse(204, "")
					default:
						t.Error("unexpected route", r.URL.Path)
						return leaseResponse(400, "")
					}
				})
				real := &execution.Executor{Store: store, API: api}
				executor := failingPipeline{Executor: real, run: leaseExecutor(func(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
					if job.ID == 2 {
						time.Sleep(time.Second)
						if mode == "descriptor" {
							return real.Execute(ctx, job, fence)
						}
						if mode == "execution" {
							return intake.State{}, errors.New("secret-body Bearer idk_private https://private")
						}
						return intake.State{Status: "held", HoldReason: "question"}, nil
					}
					select {
					case <-ctx.Done():
						t.Error("one job's error canceled peer")
						return intake.State{}, ctx.Err()
					case <-time.After(40 * time.Second):
					}
					if err := fence(ctx); err != nil {
						return intake.State{}, err
					}
					return intake.State{Status: "held", HoldReason: "question"}, nil
				})}
				runner := Runner{API: api, Executor: executor, Capacity: 2, Now: time.Now, PollInterval: time.Second, RenewInterval: time.Second}
				if err := runner.Run(ctx); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if peerSaved.Load() != 1 {
					t.Fatal("peer result not saved")
				}
				saved, err := store.Load(2)
				if err != nil || saved != nil {
					t.Fatal("runtime synthesized an execution result", saved, err)
				}
				wantReports := int32(1)
				if mode == "report_unavailable" {
					wantReports = 13
				}
				if errorReports.Load() != wantReports {
					t.Fatal("unexpected result submission", errorReports.Load())
				}
				phase := "execution"
				if strings.HasPrefix(mode, "report_") {
					phase = "state_report"
				}
				if !strings.Contains(logs.String(), "job=2 event=result_unavailable phase="+phase) || !strings.Contains(logs.String(), "job=2 event=job_error reason=") {
					t.Fatal("missing safe job/phase classification", logs.String())
				}
				for _, forbidden := range []string{"secret", "Bearer", "idk_", "https://"} {
					if strings.Contains(logs.String(), forbidden) {
						t.Fatal("unsafe error log", forbidden)
					}
				}
			})
		})
	}
}

func TestExecutorErrorNeverReportsAfterLeaseLossOrExpiry(t *testing.T) {
	for _, failure := range []string{"lease_lost", "lease_expired"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store, err := journal.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				initial := assignment(time.Now())
				initial.Lease.ExpiresAt = time.Now().Add(3 * time.Second)
				var states atomic.Int32
				api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/state") {
						states.Add(1)
						return leaseResponse(204, "")
					}
					if failure == "lease_lost" {
						return leaseResponse(409, `{"code":"lease_lost"}`)
					}
					return leaseResponse(503, "")
				})
				real := &execution.Executor{API: api, Store: store}
				executor := failingPipeline{Executor: real, run: leaseExecutor(func(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
					if failure == "lease_lost" {
						return intake.State{}, fence(ctx)
					}
					<-ctx.Done()
					return intake.State{}, errors.New("secret-error")
				})}
				runner := Runner{API: api, Executor: executor, Now: time.Now, RenewInterval: time.Second}
				err = runner.execute(context.Background(), initial)
				if jobErrorKind(err) != failure {
					t.Fatal("lease classification changed", err)
				}
				saved, err := store.Load(2)
				if err != nil || saved != nil || states.Load() != 0 {
					t.Fatal("lost lease saved or published generic failure", saved, err, states.Load())
				}
			})
		})
	}
}

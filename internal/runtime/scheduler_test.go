package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/requeststore"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type poolAPI struct {
	mu                   sync.Mutex
	queue                []intake.Job
	active               map[int64]string
	maxTotal, maxA, maxB int
	capacitySeen         []int
	states               int
	cancel               context.CancelFunc
}

func (p *poolAPI) Config(context.Context) (intake.ConfigSnapshot, error) {
	return intake.ConfigSnapshot{}, nil
}
func (p *poolAPI) Heartbeat(context.Context, intake.HostStatus) error { return nil }
func (p *poolAPI) Next(context.Context, int) (intake.Assignment, error) {
	return intake.Assignment{}, errors.New("repo-filtered next is required")
}
func (p *poolAPI) NextFor(ctx context.Context, capacity int, repos []string) (intake.Assignment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.capacitySeen = append(p.capacitySeen, capacity)
	for index, job := range p.queue {
		allowed := false
		for _, repo := range repos {
			if strings.EqualFold(repo, job.Repo) {
				allowed = true
			}
		}
		if !allowed {
			continue
		}
		p.queue = append(p.queue[:index], p.queue[index+1:]...)
		p.active[job.ID] = job.Repo
		a, b := 0, 0
		for _, repo := range p.active {
			if repo == "a/repo" {
				a++
			} else {
				b++
			}
		}
		if len(p.active) > p.maxTotal {
			p.maxTotal = len(p.active)
		}
		if a > p.maxA {
			p.maxA = a
		}
		if b > p.maxB {
			p.maxB = b
		}
		return intake.Assignment{Job: &job, Lease: &intake.Lease{Epoch: job.ID, ExpiresAt: time.Now().Add(time.Hour)}}, nil
	}
	return intake.Assignment{}, nil
}
func (p *poolAPI) Renew(ctx context.Context, id, epoch int64) (intake.Assignment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return intake.Assignment{Job: &intake.Job{ID: id}, Lease: &intake.Lease{Epoch: epoch, ExpiresAt: time.Now().Add(time.Hour)}}, nil
}
func (p *poolAPI) State(ctx context.Context, id, epoch int64, state intake.State) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.active, id)
	p.states++
	if p.states == 4 && p.cancel != nil {
		p.cancel()
	}
	return nil
}

type poolExecutor struct {
	started   chan int64
	release   map[int64]chan struct{}
	cancelled chan int64
}

func (poolExecutor) Ready() bool { return true }
func (e poolExecutor) Execute(ctx context.Context, job intake.Job, _ func(context.Context) error) (intake.State, error) {
	e.started <- job.ID
	select {
	case <-ctx.Done():
		if e.cancelled != nil {
			e.cancelled <- job.ID
		}
		return intake.State{}, ctx.Err()
	case <-e.release[job.ID]:
		return intake.State{Status: "held", HoldReason: "question"}, nil
	}
}
func receiveID(t *testing.T, ch <-chan int64) int64 {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("実行開始を待機中に停止しました")
		return 0
	}
}
func TestPoolHonorsHostAndRepositoryLimitsAndIdleRequestWindows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &poolAPI{queue: []intake.Job{{ID: 1, Repo: "a/repo"}, {ID: 2, Repo: "a/repo"}, {ID: 3, Repo: "a/repo"}, {ID: 4, Repo: "b/repo"}}, active: map[int64]string{}, cancel: cancel}
	executor := poolExecutor{started: make(chan int64, 4), release: map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{}), 3: make(chan struct{}), 4: make(chan struct{})}}
	idleB := make(chan struct{}, 1)
	runner := Runner{API: api, Executor: executor, Capacity: 3, RepoLimits: map[string]int{"a/repo": 2, "b/repo": 1}, PollInterval: time.Hour, HeartbeatInterval: time.Hour, RenewInterval: time.Hour, MaintenanceFor: func(ctx context.Context, repos []string) error {
		if len(repos) == 1 && repos[0] == "b/repo" {
			select {
			case idleB <- struct{}{}:
			default:
			}
		}
		return nil
	}}
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	started := map[int64]bool{}
	for i := 0; i < 3; i++ {
		started[receiveID(t, executor.started)] = true
	}
	if !started[1] || !started[2] || !started[4] || started[3] {
		t.Fatal("repo別上限を守らず開始しました", started)
	}
	close(executor.release[4])
	select {
	case <-idleB:
	case <-time.After(5 * time.Second):
		t.Fatal("別repoが動いている間の正規の依頼窓が開きません")
	}
	close(executor.release[1])
	if id := receiveID(t, executor.started); id != 3 {
		t.Fatal("解放したrepo枠を使いません", id)
	}
	close(executor.release[2])
	close(executor.release[3])
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("プールが停止しません")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.maxTotal != 3 || api.maxA != 2 || api.maxB != 1 || api.states != 4 {
		t.Fatal("同時実行の上限が異なります", api.maxTotal, api.maxA, api.maxB, api.states)
	}
	for _, capacity := range api.capacitySeen {
		if capacity != 3 {
			t.Fatal("空き枠を総capacityとして送信しました", capacity)
		}
	}
}
func TestPoolStopCancelsAllParallelJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &poolAPI{queue: []intake.Job{{ID: 1, Repo: "a/repo"}, {ID: 2, Repo: "a/repo"}}, active: map[int64]string{}}
	executor := poolExecutor{started: make(chan int64, 2), release: map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}, cancelled: make(chan int64, 2)}
	runner := Runner{API: api, Executor: executor, Capacity: 2, RepoLimits: map[string]int{"a/repo": 2}, PollInterval: time.Hour}
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	receiveID(t, executor.started)
	receiveID(t, executor.started)
	cancel()
	receiveID(t, executor.cancelled)
	receiveID(t, executor.cancelled)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("並列停止を待ち終えません")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.states != 0 {
		t.Fatal("停止後に完了を送信しました")
	}
}

func TestBusyRepositoryStillGetsWorktreeFreeRequestWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &poolAPI{queue: []intake.Job{{ID: 1, Repo: "a/repo"}, {ID: 2, Repo: "b/repo"}}, active: map[int64]string{}, cancel: cancel}
	executor := poolExecutor{started: make(chan int64, 2), release: map[int64]chan struct{}{1: make(chan struct{}), 2: make(chan struct{})}}
	busyA := make(chan struct{}, 1)
	runner := Runner{API: api, Executor: executor, Capacity: 3, RepoLimits: map[string]int{"a/repo": 1, "b/repo": 1}, PollInterval: time.Hour, HeartbeatInterval: time.Hour, RenewInterval: time.Hour,
		MaintenanceFor: func(context.Context, []string) error { return nil },
		BusyMaintenanceFor: func(ctx context.Context, repos []string) error {
			for _, repo := range repos {
				if repo == "a/repo" {
					select {
					case busyA <- struct{}{}:
					default:
					}
				}
			}
			return nil
		}}
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	receiveID(t, executor.started)
	receiveID(t, executor.started)
	close(executor.release[2])
	select {
	case <-busyA:
	case <-time.After(5 * time.Second):
		t.Fatal("ジョブが動いているrepoでも、作業ツリーを使わない依頼の窓が開くはずです")
	}
	close(executor.release[1])
	cancel()
	<-done
}

func TestFullHostStillGetsBusyRequestWindowForAllRepositories(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := &poolAPI{queue: []intake.Job{{ID: 1, Repo: "a/repo"}}, active: map[int64]string{}, cancel: cancel}
	executor := poolExecutor{started: make(chan int64, 1), release: map[int64]chan struct{}{1: make(chan struct{})}}
	windows := make(chan []string, 8)
	idleCalls := make(chan struct{}, 8)
	wake := make(chan struct{}, 1)
	runner := Runner{API: api, Executor: executor, Capacity: 1, RepoLimits: map[string]int{"a/repo": 1, "b/repo": 1}, PollInterval: time.Hour, HeartbeatInterval: time.Hour, RenewInterval: time.Hour, Wake: wake,
		MaintenanceFor: func(context.Context, []string) error {
			idleCalls <- struct{}{}
			return nil
		},
		BusyMaintenanceFor: func(_ context.Context, repos []string) error {
			sorted := slices.Sorted(slices.Values(repos))
			select {
			case windows <- sorted:
			default:
			}
			return nil
		}}
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	receiveID(t, executor.started)
	for len(idleCalls) > 0 {
		<-idleCalls
	}
	for len(windows) > 0 {
		<-windows
	}
	wake <- struct{}{}
	select {
	case got := <-windows:
		if !slices.Equal(got, []string{"a/repo", "b/repo"}) {
			t.Fatalf("枠が埋まったPCでは全リポジトリの依頼を受けるはずです: %v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("枠が埋まっていても、待たせない依頼の窓が開くはずです")
	}
	if len(idleCalls) != 0 {
		t.Fatal("枠が埋まっている間は作業ツリー掃除を含む通常の窓を開かないはずです")
	}
	close(executor.release[1])
	cancel()
	<-done
}

// Every fatal acquisition/validation category uses the same bounded wg drain.
// Temporary acquisition outages are covered separately by lease_acquisition_test.
func TestSchedulerFatalErrorsDrainExecutionRenewalAndReports(t *testing.T) {
	for _, failure := range []string{"acquisition_rejected", "contract_mismatch", "invalid_assignment", "assignment_epoch_mismatch", "duplicate_assignment", "repository_mismatch", "requeue_failed", "delayed_completion"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				var next, renew, reports, fences atomic.Int32
				api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/api/v1/client-config":
						return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
					case "/api/v1/hosts/fixture":
						var status intake.HostStatus
						_ = json.NewDecoder(r.Body).Decode(&status)
						if failure != "delayed_completion" && time.Since(start) > 0 && status.Ready {
							t.Error("draining worker remained ready")
						}
						return leaseResponse(204, "")
					case "/api/v1/jobs/next":
						n := next.Add(1)
						if n <= 2 {
							body := renewedJSON(time.Now().Add(time.Minute))
							if n == 2 {
								body = strings.ReplaceAll(body, `"id":2`, `"id":3`)
							}
							return leaseResponse(200, body)
						}
						switch failure {
						case "invalid_assignment":
							return leaseResponse(200, `{"job":{"id":4,"repo":"a/b"},"lease":null}`)
						case "assignment_epoch_mismatch":
							return leaseResponse(200, strings.ReplaceAll(renewedJSON(time.Now().Add(time.Minute)), `"leaseEpoch":0`, `"leaseEpoch":99`))
						case "duplicate_assignment":
							return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
						case "repository_mismatch", "requeue_failed":
							return leaseResponse(200, strings.ReplaceAll(strings.ReplaceAll(renewedJSON(time.Now().Add(time.Minute)), `"id":2`, `"id":4`), `a/b`, `c/d`))
						case "contract_mismatch":
							return leaseResponse(503, `{"code":"contract_mismatch"}`)
						case "delayed_completion":
							time.Sleep(45 * time.Second)
						}
						return leaseResponse(403, `{"code":"secret-unchecked-code"}`)
					case "/api/v1/jobs/4/state":
						if failure == "requeue_failed" {
							return leaseResponse(400, "")
						}
						return leaseResponse(204, "")
					case "/api/v1/jobs/2/lease", "/api/v1/jobs/3/lease":
						renew.Add(1)
						body := renewedJSON(time.Now().Add(time.Minute))
						if strings.Contains(r.URL.Path, "/3/") {
							body = strings.ReplaceAll(body, `"id":2`, `"id":3`)
						}
						return leaseResponse(200, body)
					case "/api/v1/jobs/2/state", "/api/v1/jobs/3/state":
						select {
						case <-r.Context().Done():
							t.Error("report canceled during drain")
							return nil, r.Context().Err()
						case <-time.After(10 * time.Second):
						}
						reports.Add(1)
						return leaseResponse(204, "")
					default:
						t.Error("unexpected route", r.URL.Path)
						return leaseResponse(400, "")
					}
				})
				runner := Runner{API: api, Capacity: 3, RepoLimits: map[string]int{"a/b": 3}, Now: time.Now, RenewInterval: time.Second, HeartbeatInterval: time.Second,
					Executor: leaseExecutor(func(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
						select {
						case <-ctx.Done():
							t.Error("fatal acquisition canceled owned job before drain")
							return intake.State{}, ctx.Err()
						case <-time.After(30 * time.Second):
						}
						if err := fence(ctx); err != nil {
							return intake.State{}, err
						}
						fences.Add(1)
						return intake.State{Status: "held", HoldReason: "question"}, nil
					}),
				}
				if err := runner.Run(context.Background()); err == nil {
					t.Fatal("fatal error was lost")
				} else if strings.Contains(err.Error(), "secret-unchecked-code") {
					t.Fatal("CLI error exposes unchecked server code")
				} else {
					message := map[string]string{
						"acquisition_rejected":      "受付APIがHTTP 403を返しました",
						"contract_mismatch":         "受付APIがHTTP 503を返しました",
						"invalid_assignment":        "仕事のIDまたはleaseが不正です",
						"assignment_epoch_mismatch": "配布の仕事とleaseのepochが一致しません",
						"duplicate_assignment":      "実行中の仕事が重複して配布されました",
						"repository_mismatch":       "受付の配布が設定のリポジトリ枠と一致しません",
						"requeue_failed":            "受付APIがHTTP 400を返しました",
						"delayed_completion":        "受付APIがHTTP 403を返しました",
					}[failure]
					if !strings.Contains(err.Error(), message) {
						t.Fatal("CLI error lost its original Japanese explanation", err)
					}
				}
				want := 40 * time.Second
				if failure == "delayed_completion" {
					want = 45 * time.Second
				}
				if time.Since(start) != want || next.Load() != 3 || reports.Load() != 2 || fences.Load() != 2 || renew.Load() < 4 {
					t.Fatal("drain did not finish both reports without reacquisition", time.Since(start), next.Load(), reports.Load(), fences.Load(), renew.Load())
				}
			})
		})
	}
}

func TestSchedulerFatalDrainDeadlineCancelsRemainingJobs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var next, renew, states, canceled atomic.Int32
		var output bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&output)
		defer log.SetOutput(previous)
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				if next.Add(1) == 1 {
					return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
				}
				return leaseResponse(403, "")
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
		runner := Runner{API: api, Capacity: 2, Now: time.Now, RenewInterval: time.Second, Executor: fakeExecutor{fn: func(ctx context.Context, _ func(context.Context) error) (intake.State, error) {
			<-ctx.Done()
			canceled.Add(1)
			return intake.State{}, ctx.Err()
		}}}
		if err := runner.Run(context.Background()); err == nil {
			t.Fatal("lost fatal acquisition error")
		}
		if shutdownDrainLimit != 5*time.Minute || time.Since(start) != shutdownDrainLimit || canceled.Load() != 1 || states.Load() != 0 || next.Load() != 2 || renew.Load() < 2 {
			t.Fatal("incorrect bounded drain", time.Since(start), canceled.Load(), states.Load(), next.Load(), renew.Load())
		}
		if !strings.Contains(output.String(), "event=drain_deadline reason=acquisition_rejected") {
			t.Fatal("missing deadline log", output.String())
		}
	})
}

func TestSchedulerCanceledExecutorCannotBlockShutdownForever(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		release := make(chan struct{})
		var next atomic.Int32
		api := &fakeAPI{}
		// Keep a valid lease so only the scheduler's independent deadline stops it.
		initial := assignment(time.Now())
		initial.Lease.ExpiresAt = time.Now().Add(time.Hour)
		wrapped := &shutdownAPI{fakeAPI: api, initial: initial, next: &next}
		runner := Runner{API: wrapped, Capacity: 2, Now: time.Now, Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
			<-release
			return intake.State{Status: "held"}, nil
		}}}
		if err := runner.Run(context.Background()); err == nil {
			t.Fatal("lost error")
		}
		if time.Since(start) != shutdownDrainLimit+shutdownCancelLimit {
			t.Fatal("unbounded cancellation join", time.Since(start))
		}
		close(release)
		synctest.Wait()
	})
}

type shutdownAPI struct {
	*fakeAPI
	initial intake.Assignment
	next    *atomic.Int32
}

func (s *shutdownAPI) Next(context.Context, int) (intake.Assignment, error) {
	if s.next.Add(1) == 1 {
		return s.initial, nil
	}
	return intake.Assignment{}, &intake.APIError{Status: 403}
}
func (s *shutdownAPI) Renew(context.Context, int64, int64) (intake.Assignment, error) {
	return s.initial, nil
}

func TestPoolReservesSlotAndRepoDuringFullTest(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		t.Run(fmt.Sprintf("capacity=%d", capacity), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				api := &poolAPI{
					queue:  []intake.Job{{ID: 1, Repo: "a/repo"}, {ID: 2, Repo: "b/repo"}, {ID: 3, Repo: "b/repo"}},
					active: map[int64]string{},
				}
				jobs := poolExecutor{started: make(chan int64, 3), release: map[int64]chan struct{}{
					1: make(chan struct{}), 2: make(chan struct{}), 3: make(chan struct{}),
				}}
				testStarted, releaseTest, reportStarted, releaseReport := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				requested := false
				var busy [][]string
				wake := make(chan struct{}, 1)
				runner := Runner{
					API: api, Executor: jobs, Capacity: capacity, Wake: wake,
					PollInterval: time.Hour, MaintenanceInterval: time.Hour,
					RepoLimits: map[string]int{"a/repo": 1, "b/repo": capacity},
					BusyMaintenanceFor: func(_ context.Context, repos []string) error {
						busy = append(busy, repos)
						return nil
					},
					MaintenanceWithFullTest: func(ctx context.Context, eligible []string, allowFullTest bool, start requeststore.StartFullTest) error {
						if requested || !allowFullTest || !slices.Contains(eligible, "a/repo") {
							return nil
						}
						requested = true
						run := func(ctx context.Context) (func(context.Context) error, error) {
							close(testStarted)
							select {
							case <-releaseTest:
							case <-ctx.Done():
								return nil, ctx.Err()
							}
							return func(ctx context.Context) error {
								close(reportStarted)
								select {
								case <-releaseReport:
									return nil
								case <-ctx.Done():
									return ctx.Err()
								}
							}, nil
						}
						if start("a/repo", run) {
							return nil
						}
						send, err := run(ctx)
						if err != nil {
							return err
						}
						return send(ctx)
					},
				}
				done := make(chan error, 1)
				go func() { done <- runner.Run(ctx) }()
				<-testStarted
				synctest.Wait()
				if capacity == 2 && receiveID(t, jobs.started) != 2 {
					t.Fatal("the spare slot did not go to the other repository")
				}
				wake <- struct{}{}
				synctest.Wait()
				if len(jobs.started) != 0 {
					t.Fatal("the full test did not reserve its slot and repository")
				}
				if capacity == 2 && (len(busy) != 1 || !slices.Equal(busy[0], []string{"b/repo"})) {
					t.Fatalf("busy window during full test: %v", busy)
				}
				close(releaseTest)
				<-reportStarted
				synctest.Wait()
				if len(jobs.started) != 0 {
					t.Fatal("acquired a job before the request result was reported")
				}
				close(releaseReport)
				if receiveID(t, jobs.started) != 1 {
					t.Fatal("the full-test repository was not available after reporting")
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestPoolAcquiresWaitingJobBetweenFullTests(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const repo = "a/repo"
		api := &poolAPI{
			queue:  []intake.Job{{ID: 1, Repo: repo}},
			active: map[int64]string{},
		}
		releaseJob, releaseFirstTest := make(chan struct{}), make(chan struct{})
		jobs := poolExecutor{started: make(chan int64, 1), release: map[int64]chan struct{}{1: releaseJob}}
		tests := make(chan int, 2)
		requested := 0
		runner := Runner{
			API: api, Executor: jobs, Capacity: 2, RepoLimits: map[string]int{repo: 1},
			PollInterval: time.Hour, MaintenanceInterval: time.Hour,
			MaintenanceWithFullTest: func(_ context.Context, eligible []string, allowFullTest bool, start requeststore.StartFullTest) error {
				if requested == 2 || !allowFullTest || !slices.Contains(eligible, repo) {
					return nil
				}
				number := requested + 1
				if start(repo, func(ctx context.Context) (func(context.Context) error, error) {
					tests <- number
					if number == 1 {
						select {
						case <-releaseFirstTest:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					return nil, nil
				}) {
					requested++
				}
				return nil
			},
		}
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		synctest.Wait()
		select {
		case got := <-tests:
			if got != 1 {
				t.Fatalf("first full test = %d", got)
			}
		default:
			t.Fatal("first full test did not start")
		}

		close(releaseFirstTest)
		synctest.Wait()
		if len(tests) != 0 || len(jobs.started) != 1 {
			t.Fatal("waiting job did not start before the second full test")
		}
		close(releaseJob)
		synctest.Wait()
		select {
		case got := <-tests:
			if got != 2 {
				t.Fatalf("second full test = %d", got)
			}
		default:
			t.Fatal("second full test did not start after the job completed")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestPoolProcessesSuccessiveFullTestsWhileAcquisitionIsHeld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		const repo = "a/repo"
		api := &poolAPI{queue: []intake.Job{{ID: 1, Repo: repo}}, active: map[int64]string{}}
		completed := make(chan int, 2)
		requested := 0
		runner := Runner{
			API: api, Executor: fakeExecutor{}, Capacity: 2, RepoLimits: map[string]int{repo: 1},
			CanAcquire: func() bool { return false }, PollInterval: time.Second,
			MaintenanceWithFullTest: func(_ context.Context, eligible []string, allowFullTest bool, start requeststore.StartFullTest) error {
				if requested == 2 || !allowFullTest || !slices.Contains(eligible, repo) {
					return nil
				}
				number := requested + 1
				if start(repo, func(context.Context) (func(context.Context) error, error) {
					return func(context.Context) error {
						completed <- number
						return nil
					}, nil
				}) {
					requested++
				}
				return nil
			},
		}
		done := make(chan error, 1)
		go func() { done <- runner.Run(ctx) }()
		synctest.Wait()
		time.Sleep(time.Second)
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(completed) != 2 || <-completed != 1 || <-completed != 2 {
			t.Fatal("both full test results were not sent in order while acquisition was held")
		}
		if len(api.capacitySeen) != 0 {
			t.Fatal("requested a job while acquisition was held")
		}
	})
}

func TestFullTestResultPostUsesRemainingAcquisitionDrainTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var posts int
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				return leaseResponse(403, "")
			case "/api/v1/requests/test/result":
				posts++
				postStart := time.Now()
				<-r.Context().Done()
				if elapsed := time.Since(postStart); elapsed != 5*time.Second {
					t.Errorf("result POST used %s of the remaining 5s", elapsed)
				}
				return nil, r.Context().Err()
			default:
				t.Errorf("unexpected route %s", r.URL.Path)
				return leaseResponse(400, "")
			}
		})
		var deliveryErr error
		runner := Runner{API: api, Capacity: 2, RepoLimits: map[string]int{"a/repo": 1, "b/repo": 1},
			Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
				t.Error("no job should be acquired")
				return intake.State{}, nil
			}},
			MaintenanceWithFullTest: func(_ context.Context, _ []string, _ bool, reserve requeststore.StartFullTest) error {
				if !reserve("a/repo", func(context.Context) (func(context.Context) error, error) {
					time.Sleep(shutdownDrainLimit - 5*time.Second)
					return func(ctx context.Context) error {
						return api.Call(ctx, "POST", "/api/v1/requests/test/result", map[string]any{"result": "passed"}, nil)
					}, nil
				}) {
					t.Error("full-test reservation refused")
				}
				return nil
			},
			OnMaintenanceError: func(err error) { deliveryErr = err },
		}
		err := runner.Run(t.Context())
		var stopped *schedulerStopError
		if !errors.As(err, &stopped) || stopped.reason != "acquisition_rejected" || posts != 1 || deliveryErr == nil || time.Since(start) != shutdownDrainLimit {
			t.Fatalf("drain elapsed=%s posts=%d delivery=%v stop=%v", time.Since(start), posts, deliveryErr, err)
		}
	})
}

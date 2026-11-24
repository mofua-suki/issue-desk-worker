package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"issue-desk-worker/internal/intake"
)

func TestAcquisitionRecoversWhileJobRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var mu sync.Mutex
		var attempts []time.Time
		var heartbeats []intake.HostStatus
		started := make(chan int64, 2)
		release := make(chan struct{})
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				var status intake.HostStatus
				if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
					t.Error(err)
				}
				mu.Lock()
				heartbeats = append(heartbeats, status)
				mu.Unlock()
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				mu.Lock()
				attempts = append(attempts, time.Now())
				n := len(attempts)
				mu.Unlock()
				switch n {
				case 1:
					return leaseResponse(200, renewedJSON(start.Add(time.Minute)))
				case 2:
					return leaseResponse(503, "")
				case 3:
					return leaseResponse(200, strings.ReplaceAll(renewedJSON(start.Add(time.Minute)), `"id":2`, `"id":3`))
				default:
					return leaseResponse(200, `{"job":null,"lease":null}`)
				}
			case "/api/v1/jobs/2/lease", "/api/v1/jobs/3/lease":
				body := minimalRenewalJSON(time.Now().Add(time.Minute))
				if strings.Contains(r.URL.Path, "/3/") {
					body = strings.ReplaceAll(body, `"id":2`, `"id":3`)
				}
				return leaseResponse(200, body)
			case "/api/v1/jobs/2/state", "/api/v1/jobs/3/state":
				return leaseResponse(204, "")
			default:
				t.Error("unexpected route", r.URL.Path)
				return leaseResponse(400, "")
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		r := Runner{API: api, Capacity: 2, PollInterval: time.Hour, HeartbeatInterval: time.Hour, RenewInterval: time.Minute,
			Executor: leaseExecutor(func(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
				started <- job.ID
				select {
				case <-release:
					return intake.State{Status: "held"}, nil
				case <-ctx.Done():
					t.Error("owned job canceled before release")
					return intake.State{}, ctx.Err()
				}
			}),
		}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		synctest.Wait()
		if id := <-started; id != 2 {
			t.Fatal("first job", id)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if id := <-started; id != 3 {
			t.Fatal("second job", id)
		}
		mu.Lock()
		gotAttempts := append([]time.Time(nil), attempts...)
		gotHeartbeats := append([]intake.HostStatus(nil), heartbeats...)
		mu.Unlock()
		if len(gotAttempts) != 3 || gotAttempts[2].Sub(gotAttempts[1]) != time.Second {
			t.Fatal("attempts", gotAttempts)
		}
		if len(gotHeartbeats) < 3 || !gotHeartbeats[0].Ready || gotHeartbeats[1].Ready || !gotHeartbeats[1].Busy || !gotHeartbeats[2].Ready || !gotHeartbeats[2].Busy || gotHeartbeats[2].Checks["ok"] != true {
			t.Fatal("heartbeats", gotHeartbeats)
		}
		close(release)
		synctest.Wait()
		cancel()
		<-done
	})
}

func TestRecoveryHeartbeatDoesNotBlockAcquiredJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		renewed := make(chan struct{}, 1)
		heartbeat := make(chan intake.HostStatus, 1)
		release := make(chan struct{})
		attempts := 0
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				var status intake.HostStatus
				if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
					t.Error(err)
				}
				if status.Ready && status.Busy {
					heartbeat <- status
					<-release
				}
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				attempts++
				if attempts == 1 {
					return leaseResponse(503, "")
				}
				return leaseResponse(200, renewedJSON(time.Now().Add(5*time.Second)))
			case "/api/v1/jobs/2/lease":
				renewed <- struct{}{}
				return leaseResponse(200, minimalRenewalJSON(time.Now().Add(time.Minute)))
			case "/api/v1/jobs/2/state":
				return leaseResponse(204, "")
			default:
				t.Error("unexpected route", r.URL.Path)
				return leaseResponse(400, "")
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		r := Runner{API: api, Capacity: 1, PollInterval: time.Hour, HeartbeatInterval: time.Hour, RenewInterval: time.Second,
			Executor: leaseExecutor(func(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
				close(started)
				<-release
				return intake.State{Status: "held"}, nil
			}),
		}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		time.Sleep(7 * time.Second)
		synctest.Wait()
		select {
		case status := <-heartbeat:
			if !status.Ready || !status.Busy || status.Implementing != 1 {
				t.Fatal("recovery heartbeat", status)
			}
		default:
			t.Fatal("recovery heartbeat not sent")
		}
		select {
		case <-started:
		default:
			t.Fatal("job did not start while heartbeat was blocked")
		}
		select {
		case <-renewed:
		default:
			t.Fatal("lease was not renewed while heartbeat was blocked")
		}
		close(release)
		cancel()
		<-done
	})
}

func TestDelayedHeartbeatCoalescesLatestStateWithoutBlockingAcquisition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		releaseHeartbeat := make(chan struct{})
		releaseJob := make(chan struct{})
		started := make(chan struct{})
		var statuses []intake.HostStatus
		attempts := 0
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				var status intake.HostStatus
				if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
					t.Error(err)
				}
				if !status.Ready && len(statuses) == 1 {
					<-releaseHeartbeat
				}
				statuses = append(statuses, status)
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				attempts++
				switch attempts {
				case 1:
					return leaseResponse(503, "")
				case 2:
					return leaseResponse(200, `{"job":null,"lease":null}`)
				case 3:
					return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
				default:
					return leaseResponse(503, "")
				}
			case "/api/v1/jobs/2/lease":
				return leaseResponse(200, minimalRenewalJSON(time.Now().Add(time.Minute)))
			case "/api/v1/jobs/2/state":
				return leaseResponse(204, "")
			default:
				t.Error("unexpected route", r.URL.Path)
				return leaseResponse(400, "")
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		r := Runner{API: api, Capacity: 2, PollInterval: time.Second, HeartbeatInterval: time.Hour, RenewInterval: time.Minute,
			Executor: leaseExecutor(func(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
				close(started)
				<-releaseJob
				return intake.State{Status: "held"}, nil
			})}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		select {
		case <-started:
		default:
			t.Fatal("acquisition blocked by heartbeat")
		}
		close(releaseJob)
		synctest.Wait()
		close(releaseHeartbeat)
		synctest.Wait()
		if len(statuses) < 3 {
			t.Fatal("missing latest heartbeat", statuses)
		}
		last := statuses[len(statuses)-1]
		if last.Ready || last.Busy || last.Implementing != 0 {
			t.Fatal("stale final heartbeat", statuses)
		}
		cancel()
		<-done
	})
}

func TestDelayedRecoveryHeartbeatCannotOverwriteLaterFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		releaseHeartbeat := make(chan struct{})
		heartbeatStarted := make(chan struct{})
		releaseJob := make(chan struct{})
		var statuses []intake.HostStatus
		attempts := 0
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				var status intake.HostStatus
				if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
					t.Error(err)
				}
				if status.Ready && status.Busy {
					close(heartbeatStarted)
					<-releaseHeartbeat
				}
				statuses = append(statuses, status)
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				attempts++
				if attempts == 2 {
					return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
				}
				if attempts == 3 {
					<-heartbeatStarted
				}
				return leaseResponse(503, "")
			case "/api/v1/jobs/2/lease":
				return leaseResponse(200, minimalRenewalJSON(time.Now().Add(time.Minute)))
			case "/api/v1/jobs/2/state":
				return leaseResponse(204, "")
			default:
				t.Error("unexpected route", r.URL.Path)
				return leaseResponse(400, "")
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		r := Runner{API: api, Capacity: 2, PollInterval: time.Hour, HeartbeatInterval: time.Hour, RenewInterval: time.Minute,
			Executor: leaseExecutor(func(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
				<-releaseJob
				return intake.State{Status: "held"}, nil
			})}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		time.Sleep(time.Second)
		synctest.Wait()
		if attempts < 3 {
			t.Fatal("later acquisition did not run", attempts)
		}
		close(releaseJob)
		synctest.Wait()
		close(releaseHeartbeat)
		synctest.Wait()
		if len(statuses) < 3 {
			t.Fatal("missing latest heartbeat", statuses)
		}
		last := statuses[len(statuses)-1]
		if last.Ready || last.Busy || last.Implementing != 0 {
			t.Fatal("stale final heartbeat", statuses)
		}
		cancel()
		<-done
	})
}

func TestAcquisitionBackoffRecoversWithoutJobs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var attempts []time.Time
		started := make(chan struct{}, 1)
		release := make(chan struct{})
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				mu.Lock()
				attempts = append(attempts, time.Now())
				n := len(attempts)
				mu.Unlock()
				if n <= 6 {
					return leaseResponse(503, "")
				}
				if n == 8 {
					return leaseResponse(503, "")
				}
				if n == 9 {
					return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
				}
				return leaseResponse(200, `{"job":null,"lease":null}`)
			case "/api/v1/jobs/2/lease":
				return leaseResponse(200, minimalRenewalJSON(time.Now().Add(time.Minute)))
			case "/api/v1/jobs/2/state":
				return leaseResponse(204, "")
			default:
				t.Error("unexpected route", r.URL.Path)
				return leaseResponse(400, "")
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		r := Runner{API: api, Capacity: 1, PollInterval: time.Millisecond, HeartbeatInterval: time.Hour, RenewInterval: time.Minute,
			Executor: leaseExecutor(func(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
				started <- struct{}{}
				select {
				case <-release:
					return intake.State{Status: "held"}, nil
				case <-ctx.Done():
					t.Error("job canceled")
					return intake.State{}, ctx.Err()
				}
			}),
		}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		time.Sleep(39 * time.Second)
		synctest.Wait()
		select {
		case <-started:
		default:
			t.Fatal("job not acquired after recovery")
		}
		mu.Lock()
		got := append([]time.Time(nil), attempts...)
		mu.Unlock()
		delays := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
		if len(got) != len(delays)+3 {
			t.Fatal("attempts", got)
		}
		if got[8].Sub(got[7]) != time.Second {
			t.Fatal("backoff did not reset", got)
		}
		for i, delay := range delays {
			if got[i+1].Sub(got[i]) != delay {
				t.Fatalf("delay %d: %v, want %v", i, got[i+1].Sub(got[i]), delay)
			}
		}
		close(release)
		synctest.Wait()
		cancel()
		<-done
	})
}

func TestAcquisitionRepeatedFailuresKeepOwnedJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var attempts []time.Time
		release := make(chan struct{})
		started := make(chan struct{})
		api := leaseClient(t, func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/api/v1/client-config":
				return leaseResponse(200, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{}}`)
			case "/api/v1/hosts/fixture":
				return leaseResponse(204, "")
			case "/api/v1/jobs/next":
				mu.Lock()
				attempts = append(attempts, time.Now())
				n := len(attempts)
				mu.Unlock()
				if n == 1 {
					return leaseResponse(200, renewedJSON(time.Now().Add(time.Minute)))
				}
				return leaseResponse(503, "")
			case "/api/v1/jobs/2/lease":
				return leaseResponse(200, minimalRenewalJSON(time.Now().Add(time.Minute)))
			case "/api/v1/jobs/2/state":
				return leaseResponse(204, "")
			default:
				t.Error("unexpected route", r.URL.Path)
				return leaseResponse(400, "")
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		r := Runner{API: api, Capacity: 2, PollInterval: time.Millisecond, HeartbeatInterval: time.Hour, RenewInterval: 10 * time.Second,
			Executor: leaseExecutor(func(ctx context.Context, job intake.Job, fence func(context.Context) error) (intake.State, error) {
				close(started)
				select {
				case <-release:
					return intake.State{Status: "held"}, nil
				case <-ctx.Done():
					t.Error("repeated failures canceled job")
					return intake.State{}, ctx.Err()
				}
			}),
		}
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()
		<-started
		time.Sleep(37 * time.Second)
		synctest.Wait()
		mu.Lock()
		got := append([]time.Time(nil), attempts...)
		mu.Unlock()
		if len(got) != 8 {
			t.Fatal("attempt count", len(got))
		}
		for i, delay := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
			if got[i+2].Sub(got[i+1]) != delay {
				t.Fatal("retry interval", i, got)
			}
		}
		close(release)
		synctest.Wait()
		cancel()
		<-done
	})
}

package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-worker/internal/intake"
)

type turnFixture struct {
	mu               sync.Mutex
	holder, epoch    int64
	until            time.Time
	clock            time.Time
	renews, releases []int64
	fail             int
	block            bool
	// lease is the turn's validity. Renewals come every third of it; 900ms was lost when
	// renewals were delayed while the whole suite ran in parallel.
	lease   time.Duration
	entered chan struct{}
	unblock chan struct{}
}

func setupTurnFixture(t *testing.T, f *pipelineFixture) *turnFixture {
	t.Helper()
	p := &turnFixture{block: true, lease: 5 * time.Second, entered: make(chan struct{}), unblock: make(chan struct{})}
	original := f.Executor.API
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/merge-turns/") {
			var req struct {
				JobID int64 `json:"jobId"`
				Epoch int64 `json:"turnEpoch"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", 400)
				return
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			now := time.Now()
			if !p.clock.IsZero() {
				now = p.clock
			}
			if now.After(p.until) {
				p.holder = 0
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "/acquire"):
				if p.holder != 0 {
					fmt.Fprint(w, `{"granted":false}`)
					return
				}
				p.epoch++
				p.holder = req.JobID
				p.until = now.Add(p.lease)
				fmt.Fprintf(w, `{"granted":true,"turn":{"epoch":%d,"expiresAt":%q}}`, p.epoch, time.Now().Add(p.lease).Format(time.RFC3339Nano))
			case strings.HasSuffix(r.URL.Path, "/renew"):
				p.renews = append(p.renews, req.Epoch)
				if p.fail != 0 {
					w.WriteHeader(p.fail)
					fmt.Fprint(w, `{"code":"turn_lost"}`)
					return
				}
				if p.holder != req.JobID || p.epoch != req.Epoch {
					w.WriteHeader(409)
					fmt.Fprint(w, `{"code":"turn_lost"}`)
					return
				}
				p.until = now.Add(p.lease)
				fmt.Fprintf(w, `{"turn":{"epoch":%d,"expiresAt":%q}}`, p.epoch, time.Now().Add(p.lease).Format(time.RFC3339Nano))
			case strings.HasSuffix(r.URL.Path, "/release"):
				p.releases = append(p.releases, req.Epoch)
				if p.fail != 0 {
					w.WriteHeader(500)
					fmt.Fprint(w, `{}`)
					return
				}
				if p.holder == req.JobID && p.epoch == req.Epoch {
					p.holder = 0
				}
				fmt.Fprint(w, `{}`)
			}
			return
		}
		if r.URL.Path == "/api/v1/commands" {
			p.mu.Lock()
			blocked := p.block && p.holder == 1
			if blocked {
				p.block = false
				close(p.entered)
			}
			p.mu.Unlock()
			if blocked {
				select {
				case <-p.unblock:
				case <-r.Context().Done():
					return
				}
			}
		}
		body, _ := io.ReadAll(r.Body)
		response, err := original.Explicit(r.Context(), r.Method, r.URL.RequestURI(), body)
		if err != nil {
			http.Error(w, "proxy", 500)
			return
		}
		w.WriteHeader(response.Status)
		_, _ = w.Write(response.Body)
	}))
	t.Cleanup(server.Close)
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.API = api
	return p
}

func (p *turnFixture) request(t *testing.T, f *pipelineFixture, action string, job, epoch int64) bool {
	t.Helper()
	var result struct {
		Granted bool `json:"granted"`
	}
	err := f.Executor.API.Call(context.Background(), "POST", "/api/v1/merge-turns/owner/project/"+action, map[string]any{"jobId": job, "epoch": 3, "turnEpoch": epoch}, &result)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	return result.Granted
}
func awaitTurn(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("merge preparation did not start")
	}
}
func waitRenewal(t *testing.T, p *turnFixture) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		p.mu.Lock()
		n := len(p.renews)
		p.mu.Unlock()
		if n > 0 {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("no renewal")
		}
	}
}
func startTurnMerge(f *pipelineFixture, ctx context.Context, fence func(context.Context) error) <-chan error {
	done := make(chan error, 1)
	go func() { _, err := f.Executor.Execute(ctx, f.Job, fence); done <- err }()
	return done
}

func TestMergeTurnRenewalDuringPreparation(t *testing.T) {
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	before, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil || before == nil || before.Timing.FixedTestCount != 1 {
		t.Fatalf("fixed test count before merge: record=%v err=%v", before, err)
	}
	f.CurrentPolicy = nil
	p := setupTurnFixture(t, f)
	p.mu.Lock()
	p.clock = time.Now()
	p.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startTurnMerge(f, ctx, func(context.Context) error { return nil })
	awaitTurn(t, p.entered)
	p.mu.Lock()
	initialExpiry := p.until
	p.clock = p.clock.Add(p.lease / 10)
	p.renews = nil
	p.mu.Unlock()
	waitRenewal(t, p)
	p.mu.Lock()
	// Advance past the original expiry but remain within the renewed lease.
	p.clock = initialExpiry.Add(50 * time.Millisecond)
	p.mu.Unlock()
	if p.request(t, f, "acquire", 2, 0) {
		t.Fatal("other job acquired held turn")
	}
	p.mu.Lock()
	epoch := p.epoch
	for _, e := range p.renews {
		if e != epoch {
			t.Errorf("renewed epoch %d, want %d", e, epoch)
		}
	}
	p.mu.Unlock()
	close(p.unblock)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.MergePosts != 1 || !p.request(t, f, "acquire", 2, 0) {
		t.Fatalf("merge or release failed: posts=%d", f.MergePosts)
	}
	after, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil || after == nil || after.Timing.FixedTestCount != before.Timing.FixedTestCount {
		t.Fatalf("fixed tests repeated during merge: record=%v err=%v", after, err)
	}
}

func TestMergeTurnLossCancelsPreparation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"turn_lost", "renew_failure", "lease_lost", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			f := newPipeline(t, "approve")
			prepareMergeRecovery(t, f)
			f.CurrentPolicy = nil
			p := setupTurnFixture(t, f)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			leaseLost := make(chan struct{})
			fence := func(ctx context.Context) error {
				select {
				case <-leaseLost:
					return &intake.APIError{Status: 409, Code: "lease_lost"}
				default:
					return ctx.Err()
				}
			}
			done := startTurnMerge(f, ctx, fence)
			awaitTurn(t, p.entered)
			active := f.Executor.controls.get(f.Job.ID)
			if active == nil {
				t.Fatal("missing active job control")
			}
			p.mu.Lock()
			old := p.epoch
			if kind == "turn_lost" {
				p.fail = 409
			}
			if kind == "renew_failure" {
				p.fail = 500
			}
			p.mu.Unlock()
			switch kind {
			case "lease_lost":
				close(leaseLost)
				close(p.unblock)
			case "cancel":
				cancel()
			}
			if err := <-done; err == nil {
				t.Fatal("lost turn did not stop execution")
			}
			active.mu.Lock()
			started := active.mergeStarted
			active.mu.Unlock()
			if started {
				t.Fatal("merge marked started after turn loss")
			}
			p.mu.Lock()
			n := len(p.renews)
			releases := append([]int64(nil), p.releases...)
			expiry := p.until
			p.mu.Unlock()
			if f.MergePosts != 0 || len(releases) != 1 || releases[0] != old {
				t.Fatalf("posts=%d releases=%v", f.MergePosts, releases)
			}
			time.Sleep(max(350*time.Millisecond, time.Until(expiry)+350*time.Millisecond))
			p.mu.Lock()
			if len(p.renews) != n {
				t.Errorf("renewal continued after return")
			}
			p.fail = 0
			p.mu.Unlock()
			if !p.request(t, f, "acquire", 2, 0) {
				t.Fatal("other job could not acquire expired turn")
			}
			p.request(t, f, "release", 1, old)
			if p.request(t, f, "acquire", 3, 0) {
				t.Fatal("old release removed new holder")
			}
		})
	}
}

func TestMergeTurnReacquiredOnResume(t *testing.T) {
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	p := setupTurnFixture(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := startTurnMerge(f, ctx, func(ctx context.Context) error { return ctx.Err() })
	awaitTurn(t, p.entered)
	waitRenewal(t, p)
	p.mu.Lock()
	old := p.epoch
	p.mu.Unlock()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected interruption")
	}
	p.mu.Lock()
	beforeResume := len(p.renews)
	p.mu.Unlock()
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("resume: status=%s err=%v", state.Status, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.epoch <= old || len(p.releases) != 2 || p.releases[0] != old || p.releases[1] != p.epoch || f.MergePosts != 1 {
		t.Fatalf("epoch=%d old=%d releases=%v posts=%d", p.epoch, old, p.releases, f.MergePosts)
	}
	for _, e := range p.renews[beforeResume:] {
		if e == old {
			t.Fatalf("old epoch renewed on resume: %v", p.renews)
		}
	}
}

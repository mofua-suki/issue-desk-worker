package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/providers"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// Advance the fixture's real default branch after the turn has been requested.
func advanceOnTurn(t *testing.T, f *pipelineFixture) {
	advanceFileOnTurn(t, f, "upstream.txt", "upstream\n")
}

func advanceFileOnTurn(t *testing.T, f *pipelineFixture, file, contents string) {
	t.Helper()
	original := f.Executor.API
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/acquire") {
			once.Do(func() {
				source := filepath.Join(f.root, "source")
				if err := os.WriteFile(filepath.Join(source, file), []byte(contents), 0600); err != nil {
					t.Error(err)
					return
				}
				f.git(source, "add", file)
				f.git(source, "commit", "-m", "advance upstream")
				f.git(source, "push", f.bare, "main")
			})
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
}

func TestMergeFollowHoldsTurnUntilMerged(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	p := setupTurnFixture(t, f)
	p.block = false
	advanceOnTurn(t, f)
	handler := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls/1/merge") {
			p.mu.Lock()
			if p.holder != 1 || p.epoch != 1 || len(p.releases) != 0 {
				t.Errorf("turn changed before merge: holder=%d epoch=%d releases=%v", p.holder, p.epoch, p.releases)
			}
			p.mu.Unlock()
		}
		handler.ServeHTTP(w, r)
	})
	before, _ := f.Executor.Store.Load(1)
	marker := filepath.Join(f.root, "follow-tests")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "printf x >> '"+marker+"'")
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	after, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := os.ReadFile(marker)
	if err != nil || string(runs) != "x" {
		t.Fatalf("test runs %q: %v", runs, err)
	}
	if after.Head == before.Head || after.Base == before.Base || testEvidenceReasonFromRecord(after) != "" {
		t.Fatalf("target evidence did not follow: %+v", after)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.epoch != 1 || len(p.releases) != 1 || p.releases[0] != 1 || f.MergePosts != 1 {
		t.Fatalf("epoch=%d releases=%v merges=%d", p.epoch, p.releases, f.MergePosts)
	}
}

func testEvidenceReasonFromRecord(r *journal.Record) string {
	var result TestResult
	if json.Unmarshal(r.Test, &result) != nil {
		return "invalid result"
	}
	return testEvidenceReason(result, r.Head, r.Base)
}

func TestMergeFollowBlocksOtherJob(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	p := setupTurnFixture(t, f)
	p.block = false
	advanceOnTurn(t, f)
	marker, unblock := filepath.Join(f.root, "started"), filepath.Join(f.root, "unblock")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("printf x >> '%s'; while [ ! -f '%s' ]; do sleep 0.1; done", marker, unblock))
	done := make(chan error, 1)
	go func() {
		state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
		if err == nil && state.Status != "merged" {
			err = fmt.Errorf("status %s", state.Status)
		}
		done <- err
	}()
	t.Cleanup(func() { _ = os.WriteFile(unblock, []byte("ok"), 0600) })
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test did not start")
		}
		select {
		case err := <-done:
			t.Fatalf("merge ended before the followed test started: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	// Freeze the fixture clock, require a renewal while the followed test runs, then
	// move past the lease held when the test started: without renewals during the test
	// the turn would have expired there.
	p.mu.Lock()
	p.clock = time.Now()
	initialExpiry := p.until
	p.clock = p.clock.Add(p.lease / 10)
	p.renews = nil
	p.mu.Unlock()
	// The first renewal may have been sent just before the test started; the second
	// comes a full interval later and so certainly during the test.
	renewDeadline := time.After(20 * time.Second)
	for {
		p.mu.Lock()
		n := len(p.renews)
		p.mu.Unlock()
		if n >= 2 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("merge ended during the followed test: %v", err)
		case <-renewDeadline:
			t.Fatalf("renewals during the followed test: %d", n)
		case <-time.After(10 * time.Millisecond):
		}
	}
	p.mu.Lock()
	p.clock = initialExpiry.Add(50 * time.Millisecond)
	p.mu.Unlock()
	if p.request(t, f, "acquire", 2, 0) {
		t.Fatal("second job acquired while test running")
	}
	if err := os.WriteFile(unblock, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if runs, err := os.ReadFile(marker); err != nil || string(runs) != "x" {
		t.Fatalf("followed tests=%q err=%v", runs, err)
	}
	if !p.request(t, f, "acquire", 2, 0) {
		t.Fatal("second job could not acquire after merge")
	}
}

func TestMergeFollowFailureReleasesBeforeRepair(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	f.AllowUsage = true
	p := setupTurnFixture(t, f)
	p.block = false
	advanceOnTurn(t, f)
	marker := filepath.Join(f.root, "attempts")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("printf x >> '%s'; exit 1", marker))
	original := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if !req.Review {
			calls++
			p.mu.Lock()
			held, released := p.holder, len(p.releases)
			p.mu.Unlock()
			if held != 0 || released != 1 {
				t.Errorf("repair started with turn held: %d releases=%d", held, released)
			}
			runs, err := os.ReadFile(marker)
			// The released failure gets one rerun outside the turn, then repair.
			if err != nil || string(runs) != "xx" {
				t.Errorf("failure not rerun exactly once before repair: %q %v", runs, err)
			}
			return providers.Answer{}, fmt.Errorf("stop repair")
		}
		return original(ctx, req)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" || calls != 1 || f.MergePosts != 0 {
		t.Fatalf("state=%+v err=%v repairs=%d merges=%d", state, err, calls, f.MergePosts)
	}
	record, err := f.Executor.Store.Load(1)
	if err != nil || record.FixedTestFailures != 1 {
		t.Fatalf("failure budget not recorded: %+v %v", record, err)
	}
	for _, kind := range []string{"environment", "missing evidence"} {
		t.Run(kind, func(t *testing.T) {
			f := newPipeline(t, "approve")
			prepareMergeRecovery(t, f)
			f.CurrentPolicy = nil
			p := setupTurnFixture(t, f)
			p.block = false
			advanceOnTurn(t, f)
			if kind == "environment" {
				f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "echo \"EACCES: permission denied, lstat '/outside/cache'\" >&2; exit 1"}
			} else {
				f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "echo changed > feature.txt")
			}
			calls := len(f.Calls)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "failed" || len(f.Calls) != calls || f.MergePosts != 0 {
				t.Fatalf("status=%s err=%v AI=%d merge=%d", state.Status, err, len(f.Calls)-calls, f.MergePosts)
			}
			if kind == "environment" && !strings.Contains(string(state.View), "environment") {
				t.Fatal("environment classification missing")
			}
			if kind == "missing evidence" && !strings.Contains(string(state.Result), "証拠") {
				t.Fatal("missing-evidence classification missing")
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if len(p.releases) != 1 {
				t.Fatalf("release attempts=%v", p.releases)
			}
		})
	}
}

func TestMergeFollowTurnRenewalAndCancellation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"turn_lost", "renew_failure", "lease_lost", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := newPipeline(t, "approve")
			prepareMergeRecovery(t, f)
			f.CurrentPolicy = nil
			p := setupTurnFixture(t, f)
			p.block = false
			advanceOnTurn(t, f)
			marker, unblock := filepath.Join(f.root, "started"), filepath.Join(f.root, "unblock")
			f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("touch '%s'; while [ ! -f '%s' ]; do sleep 0.1; done", marker, unblock))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			lost := false
			fence := func(context.Context) error {
				mu.Lock()
				defer mu.Unlock()
				if lost {
					return &intake.APIError{Status: 409, Code: "lease_lost"}
				}
				return nil
			}
			done := make(chan error, 1)
			go func() {
				state, err := f.Executor.Execute(ctx, f.Job, fence)
				if err == nil && state.Status == "merged" {
					err = fmt.Errorf("merged after cancellation")
				}
				done <- err
			}()
			t.Cleanup(func() { cancel(); _ = os.WriteFile(unblock, []byte("ok"), 0600) })
			deadline := time.Now().Add(30 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("test did not start")
				}
				select {
				case err := <-done:
					t.Fatalf("finished before the test started: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
			}
			waitRenewal(t, p)
			switch kind {
			case "turn_lost":
				p.mu.Lock()
				p.fail = 409
				p.mu.Unlock()
			case "renew_failure":
				p.mu.Lock()
				p.fail = 500
				p.mu.Unlock()
			case "lease_lost":
				mu.Lock()
				lost = true
				mu.Unlock()
				_ = os.WriteFile(unblock, []byte("ok"), 0600)
			case "cancel":
				cancel()
			}
			if err := <-done; err == nil {
				t.Fatal("cancellation did not stop execution")
			}
			p.mu.Lock()
			releases, renews := len(p.releases), len(p.renews)
			p.mu.Unlock()
			if releases != 1 || f.MergePosts != 0 {
				t.Fatalf("releases=%d merges=%d", releases, f.MergePosts)
			}
			p.mu.Lock()
			until := p.until
			p.fail = 0
			p.mu.Unlock()
			if delay := time.Until(until); delay > 0 {
				time.Sleep(delay + 20*time.Millisecond)
			}
			p.mu.Lock()
			if len(p.renews) != renews {
				p.mu.Unlock()
				t.Fatal("renewal continued after return")
			}
			p.mu.Unlock()
			if !p.request(t, f, "acquire", 2, 0) {
				t.Fatal("new holder did not acquire after expiry")
			}
			p.request(t, f, "release", 1, 1)
			p.mu.Lock()
			holder, epoch := p.holder, p.epoch
			p.mu.Unlock()
			if holder != 2 || epoch != 2 {
				t.Fatalf("old release removed new holder: holder=%d epoch=%d", holder, epoch)
			}
		})
	}
}

func TestMergeFollowConflictReleasesTurn(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	f.AllowUsage = true
	p := setupTurnFixture(t, f)
	p.block = false
	advanceFileOnTurn(t, f, "feature.txt", "upstream conflict\n")
	original := f.Executor.Call
	repairs := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if !req.Review {
			repairs++
			p.mu.Lock()
			holder, releases := p.holder, len(p.releases)
			p.mu.Unlock()
			if holder != 0 || releases != 1 {
				t.Errorf("conflict repair before release: %d %d", holder, releases)
			}
			if err := os.WriteFile(filepath.Join(req.Policy.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(req.Policy.Tree, "add", "feature.txt")
		}
		return original(ctx, req)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || repairs != 1 || f.MergePosts != 1 {
		t.Fatalf("state=%+v err=%v repairs=%d merges=%d", state, err, repairs, f.MergePosts)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.epoch != 2 || len(p.releases) != 2 {
		t.Fatalf("turns epoch=%d releases=%v", p.epoch, p.releases)
	}
}

func TestMergeFollowRecoveryUsesSavedEvidence(t *testing.T) {
	t.Parallel()
	t.Run("complete evidence reacquires", func(t *testing.T) {
		f := newPipeline(t, "approve")
		prepareMergeRecovery(t, f)
		f.CurrentPolicy = nil
		p := setupTurnFixture(t, f)
		p.block = false
		if !p.request(t, f, "acquire", 1, 0) {
			t.Fatal("initial turn not acquired")
		}
		p.request(t, f, "release", 1, 1)
		state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
		if err != nil || state.Status != "merged" || f.MergePosts != 1 {
			t.Fatalf("status=%s err=%v merges=%d", state.Status, err, f.MergePosts)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.epoch != 2 || len(p.releases) != 2 || p.releases[1] != 2 {
			t.Fatalf("old turn reused: epoch=%d releases=%v", p.epoch, p.releases)
		}
	})
	for _, kind := range []string{"missing", "mismatch"} {
		t.Run(kind, func(t *testing.T) {
			f := newPipeline(t, "approve")
			prepareMergeRecovery(t, f)
			f.CurrentPolicy = nil
			r, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "missing" {
				r.Test = nil
			} else {
				var result TestResult
				if json.Unmarshal(r.Test, &result) != nil {
					t.Fatal("invalid fixture")
				}
				result.Observation = nil
				r.Test = encode(result)
			}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			p := setupTurnFixture(t, f)
			p.block = false
			marker := filepath.Join(f.root, "retest")
			f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("printf x >> '%s'", marker))
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			runs, readErr := os.ReadFile(marker)
			saved, loadErr := f.Executor.Store.Load(1)
			if err != nil || state.Status != "held" || f.MergePosts != 0 || readErr != nil || string(runs) != "x" || loadErr != nil || testEvidenceReasonFromRecord(saved) != "" {
				t.Fatalf("status=%s err=%v runs=%q read=%v load=%v", state.Status, err, runs, readErr, loadErr)
			}
		})
	}
}

func TestMergeWithoutBaseAdvanceKeepsOrder(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	p := setupTurnFixture(t, f)
	p.block = false
	marker := filepath.Join(f.root, "extra-test")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("touch '%s'", marker))
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || f.MergePosts != 1 {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("unchanged target was tested again")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.epoch != 1 || len(p.releases) != 1 {
		t.Fatalf("turns=%d releases=%v", p.epoch, p.releases)
	}
}

func TestMergeFollowPreservesReviewAndReleaseGates(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"requested", "dismissed", "failure", "incomplete"} {
		t.Run("paged reviews "+mode, func(t *testing.T) {
			f := newPipeline(t, "approve")
			prepareMergeRecovery(t, f)
			f.CurrentPolicy = nil
			p := setupTurnFixture(t, f)
			p.block = false
			advanceOnTurn(t, f)
			f.GiteaOverride = pagedReviewResponse(mode)
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if mode == "dismissed" {
				if err != nil || state.Status != "merged" || f.MergePosts != 1 {
					t.Fatalf("status=%s err=%v merges=%d", state.Status, err, f.MergePosts)
				}
			} else if f.MergePosts != 0 || (mode == "requested" && (err != nil || state.Status != "held")) {
				t.Fatalf("status=%s err=%v merges=%d", state.Status, err, f.MergePosts)
			}
		})
	}
	t.Run("review rejection releases before repair", func(t *testing.T) {
		f := newPipeline(t, "approve")
		prepareMergeRecovery(t, f)
		f.CurrentPolicy = nil
		f.AllowUsage = true
		record, err := f.Executor.Store.Load(1)
		if err != nil {
			t.Fatal(err)
		}
		record.ReviewBaseline = nil
		if err := f.Executor.Store.Save(record); err != nil {
			t.Fatal(err)
		}
		p := setupTurnFixture(t, f)
		p.block = false
		advanceOnTurn(t, f)
		original := f.Executor.Call
		reviews, repairs := 0, 0
		f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
			if !req.Review {
				repairs++
				p.mu.Lock()
				holder, releases := p.holder, len(p.releases)
				p.mu.Unlock()
				if holder != 0 || releases != 1 {
					t.Errorf("review repair while held: holder=%d releases=%d", holder, releases)
				}
				return providers.Answer{}, fmt.Errorf("stop repair")
			}
			reviews++
			p.mu.Lock()
			before, holder := len(p.renews), p.holder
			p.mu.Unlock()
			if holder != 1 {
				t.Error("review did not hold turn")
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				p.mu.Lock()
				n := len(p.renews)
				p.mu.Unlock()
				if n > before {
					break
				}
				if time.Now().After(deadline) {
					t.Error("turn not renewed during review")
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			answer, err := original(ctx, req)
			if err != nil {
				return answer, err
			}
			var review map[string]any
			if json.Unmarshal(answer.JSON, &review) != nil {
				t.Fatal("review fixture invalid")
			}
			review["decision"] = "hold"
			review["findings"] = []string{"需要修正"}
			review["findingKinds"] = []string{"security"}
			review["findingSeverities"] = []string{"must"}
			answer.JSON = encode(review)
			return answer, nil
		}
		state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
		if err != nil || state.Status != "failed" || reviews != 1 || repairs != 1 || f.MergePosts != 0 {
			t.Fatalf("status=%s err=%v reviews=%d repairs=%d merges=%d", state.Status, err, reviews, repairs, f.MergePosts)
		}
	})
	t.Run("external change request blocks merge", func(t *testing.T) {
		f := newPipeline(t, "approve")
		prepareMergeRecovery(t, f)
		f.CurrentPolicy = nil
		p := setupTurnFixture(t, f)
		p.block = false
		advanceOnTurn(t, f)
		marker := filepath.Join(f.root, "tested")
		f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("touch '%s'", marker))
		handler := f.server.Config.Handler
		f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls/1/reviews") {
				if _, err := os.Stat(marker); err == nil {
					_, _ = io.WriteString(w, `[{"state":"REQUEST_CHANGES","dismissed":false}]`)
					return
				}
			}
			handler.ServeHTTP(w, r)
		})
		state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
		if err != nil || state.Status != "held" || f.MergePosts != 0 {
			t.Fatalf("status=%s err=%v merges=%d", state.Status, err, f.MergePosts)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.releases) != 1 {
			t.Fatalf("release attempts=%v", p.releases)
		}
	})
	t.Run("changed PR head blocks merge", func(t *testing.T) {
		f := newPipeline(t, "approve")
		prepareMergeRecovery(t, f)
		f.CurrentPolicy = nil
		p := setupTurnFixture(t, f)
		p.block = false
		advanceOnTurn(t, f)
		marker := filepath.Join(f.root, "tested")
		f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("touch '%s'", marker))
		f.PullOverride = func(pull *gitea.Pull) {
			if _, err := os.Stat(marker); err == nil {
				pull.Head.SHA = strings.Repeat("a", 40)
			}
		}
		state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
		if err == nil && state.Status == "merged" || f.MergePosts != 0 {
			t.Fatalf("status=%s err=%v merges=%d", state.Status, err, f.MergePosts)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.releases) != 1 {
			t.Fatalf("release attempts=%v", p.releases)
		}
	})
	t.Run("changed permission policy blocks merge", func(t *testing.T) {
		f := newPipeline(t, "approve")
		prepareMergeRecovery(t, f)
		f.CurrentPolicy = nil
		p := setupTurnFixture(t, f)
		p.block = false
		advanceOnTurn(t, f)
		marker := filepath.Join(f.root, "tested")
		f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("touch '%s'", marker))
		original := f.Executor.API
		var once sync.Once
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/v1/client-config" {
				if _, err := os.Stat(marker); err == nil {
					once.Do(func() {
						f.CurrentPolicy = currentPolicy(f, true)
						f.CurrentPolicy.ReleasePolicy = &ReleasePolicy{PermissionFiles: []string{"feature.txt"}}
					})
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
		state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
		if err != nil || state.Status != "held" || f.MergePosts != 0 || !strings.Contains(string(state.View), "permissionHold") {
			t.Fatalf("status=%s err=%v merges=%d view=%s", state.Status, err, f.MergePosts, state.View)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.releases) != 1 {
			t.Fatalf("release attempts=%v", p.releases)
		}
	})

}

func TestMergeFollowMergeRefusalMaintainsTurn(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	p := setupTurnFixture(t, f)
	p.block = false
	marker := filepath.Join(f.root, "tests")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("printf x >> '%s'", marker))
	handler := f.server.Config.Handler
	attempts := 0
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls/1/merge") {
			attempts++
			p.mu.Lock()
			holder, epoch, releases := p.holder, p.epoch, len(p.releases)
			p.mu.Unlock()
			if holder != 1 || epoch != 1 || releases != 0 {
				t.Errorf("turn changed during merge attempt: holder=%d epoch=%d releases=%d", holder, epoch, releases)
			}
			if attempts == 1 {
				source := filepath.Join(f.root, "source")
				if err := os.WriteFile(filepath.Join(source, "upstream.txt"), []byte("upstream\n"), 0600); err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				f.git(source, "add", "upstream.txt")
				f.git(source, "commit", "-m", "advance upstream")
				f.git(source, "push", f.bare, "main")
				w.WriteHeader(http.StatusConflict)
				return
			}
		}
		handler.ServeHTTP(w, r)
	})
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	runs, readErr := os.ReadFile(marker)
	if err != nil || state.Status != "merged" || attempts != 2 || f.MergePosts != 1 || readErr != nil || string(runs) != "x" {
		t.Fatalf("status=%s err=%v attempts=%d merged=%d runs=%q read=%v", state.Status, err, attempts, f.MergePosts, runs, readErr)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || testEvidenceReasonFromRecord(saved) != "" {
		t.Fatal("missing merge target evidence", err)
	}
}

func TestMergeRefusalUnchangedFollowReleasesTurn(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	p := setupTurnFixture(t, f)
	p.block = false
	marker := filepath.Join(f.root, "tests")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("printf x >> '%s'", marker))
	f.MergeFailStatus, f.MergeFailBody = http.StatusConflict, `{"message":"conflict"}`
	f.PullOverride = func(pull *gitea.Pull) {
		if f.MergePosts > 0 {
			pull.Base.SHA = strings.Repeat("a", 40)
		}
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || f.MergePosts != 1 {
		t.Fatalf("status=%s err=%v merges=%d", state.Status, err, f.MergePosts)
	}
	if runs, err := os.ReadFile(marker); err == nil || !os.IsNotExist(err) {
		t.Fatalf("unexpected retest: runs=%q err=%v", runs, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.holder != 0 || len(p.releases) != 1 {
		t.Fatalf("holder=%d releases=%v", p.holder, p.releases)
	}
}

func TestMergeFollowNonConflictFailureDoesNotResolve(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	f.AllowUsage = true
	record, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	p := setupTurnFixture(t, f)
	p.block = false
	advanceOnTurn(t, f)
	original := f.Executor.API
	var once sync.Once
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/acquire") {
			once.Do(func() {
				if err := os.WriteFile(filepath.Join(record.Tree, "upstream.txt"), []byte("local untracked\n"), 0600); err != nil {
					t.Error(err)
				}
			})
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
	calls := len(f.Calls)
	_, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err == nil || errors.Is(err, gitwork.ErrConflict) || len(f.Calls) != calls || f.MergePosts != 0 {
		t.Fatalf("error=%v AI calls=%d merge=%d", err, len(f.Calls)-calls, f.MergePosts)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.releases) != 1 {
		t.Fatalf("releases=%v", p.releases)
	}
}

// pagedReviewResponse simulates a full first page and a subsequent review or read failure.
func pagedReviewResponse(mode string) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/pulls/1/reviews") {
			return false
		}
		page := r.URL.Query().Get("page")
		if page == "1" || mode == "incomplete" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "["+strings.TrimSuffix(strings.Repeat(`{"state":"APPROVED"},`, 50), ",")+"]")
		} else if mode == "failure" {
			http.Error(w, "read failure", http.StatusInternalServerError)
		} else if mode == "requested" {
			_, _ = io.WriteString(w, `[{"state":"REQUEST_CHANGES","dismissed":false}]`)
		} else {
			_, _ = io.WriteString(w, `[{"state":"REQUEST_CHANGES","dismissed":true}]`)
		}
		return true
	}
}

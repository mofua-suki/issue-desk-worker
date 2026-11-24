package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
)

func TestPublishMergeTransportFailure(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"disconnect", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			f := newPipeline(t, "approve")
			done := make(chan struct{})
			h := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/repos/owner/project/pulls/1/merge" {
					f.MergePosts++
					if failure == "disconnect" {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
					} else {
						select {
						case <-r.Context().Done():
						case <-done:
						}
					}
					return
				}
				h.ServeHTTP(w, r)
			})
			start := time.Now()
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			close(done)
			if err == nil || f.MergePosts != 1 {
				t.Fatalf("state=%+v err=%v posts=%d", state, err, f.MergePosts)
			}
			if failure == "timeout" && time.Since(start) < 30*time.Second {
				t.Fatalf("client timeout did not elapse: %v", time.Since(start))
			}
			saved, loadErr := f.Executor.Store.Load(f.Job.ID)
			if loadErr != nil || saved == nil || hasJSON(saved.FinalState) {
				t.Fatalf("unexpected final result: %v", loadErr)
			}
		})
	}
}

func TestPublishMergeRefusalBaseChangeRevalidates(t *testing.T) {
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	f.AllowUsage = true
	h := f.server.Config.Handler
	first := true
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if first && r.URL.Path == "/api/v1/repos/owner/project/pulls/1/merge" {
			first = false
			f.MergePosts++
			source := filepath.Join(f.root, "source")
			if err := os.WriteFile(filepath.Join(source, "upstream.txt"), []byte("upstream\n"), 0600); err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			f.git(source, "add", "upstream.txt")
			f.git(source, "commit", "-m", "advance base")
			f.git(source, "push", f.bare, "main")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
	marker := filepath.Join(f.root, "retested")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("test -f upstream.txt && touch '%s'", marker))
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || f.MergePosts != 2 {
		t.Fatalf("status=%s err=%v posts=%d", state.Status, err, f.MergePosts)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fixed test did not run on the updated base: %v", err)
	}
}

func TestPublishMergeRefusalSavedAndResumed(t *testing.T) {
	for _, status := range []int{403, 405, 409, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.Executor.secret = "private-value"
			f.MergeFailStatus = status
			f.MergeFailBody = `{"message":"branch policy blocks merge; private-value sk-abcdefghijklmnopqrstuvwxyz ` + strings.Repeat("x", 700) + `"}`
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "held" || state.HoldReason != "review_held" || f.MergePosts != 1 {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			var result map[string]any
			var view map[string]any
			if json.Unmarshal(state.Result, &result) != nil || json.Unmarshal(state.View, &view) != nil {
				t.Fatal("invalid result")
			}
			message, _ := result["message"].(string)
			summary, _ := result["summary"].(string)
			if !strings.Contains(message, fmt.Sprint(status)) || !strings.Contains(message, "branch policy blocks merge") || !strings.Contains(message, "[REDACTED]") || !strings.Contains(summary, message) || view["releasePending"] != true || len([]rune(message)) > 530 {
				t.Fatalf("message=%q summary=%q view=%v", message, summary, view)
			}
			saved, err := f.Executor.Store.Load(f.Job.ID)
			if err != nil || saved == nil {
				t.Fatal(err)
			}
			if strings.Contains(string(saved.FinalState), "private-value") || strings.Contains(string(saved.FinalState), "sk-abcdefghijklmnopqrstuvwxyz") {
				t.Fatal("secret in saved state")
			}
			var stored intake.State
			if json.Unmarshal(saved.FinalState, &stored) != nil || string(stored.Result) != string(state.Result) {
				t.Fatal("saved result differs")
			}
			posts, calls := f.MergePosts, len(f.Calls)
			resumed, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || string(resumed.Result) != string(state.Result) || f.MergePosts != posts || len(f.Calls) != calls {
				t.Fatalf("resume=%+v err=%v", resumed, err)
			}
		})
	}
}

func TestPublishMergeRefusalBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		applied  bool
		readFail int
		mismatch bool
		body     string
		want     string
	}{
		{"other status same reason", 403, false, 0, false, `{"message":"Please try again later"}`, "held"},
		{"empty reason", 405, false, 0, false, "", "held"},
		{"timeout status", 408, false, 0, false, "", "error"},
		{"rate limit", 429, false, 0, false, "", "error"},
		{"server error", 503, false, 0, false, "", "error"},
		{"read failure", 405, false, 503, false, "", "error"},
		{"identity mismatch", 405, false, 0, true, "", "error"},
		{"merged despite error", 405, true, 0, false, "", "merged"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.MergeFailStatus, f.MergeFailApplied, f.PullFailAfterPost = tc.status, tc.applied, tc.readFail
			f.MergeFailBody = tc.body
			if tc.mismatch {
				f.PullOverride = func(p *gitea.Pull) {
					if f.MergePosts > 0 {
						p.Number = 2
					}
				}
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if tc.want == "error" {
				if err == nil {
					t.Fatalf("expected error: %+v", state)
				}
				saved, e := f.Executor.Store.Load(f.Job.ID)
				if e != nil || saved == nil || hasJSON(saved.FinalState) {
					t.Fatal("unexpected final result", e)
				}
				return
			}
			if err != nil || state.Status != tc.want {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if f.MergePosts != 1 {
				t.Fatalf("posts=%d", f.MergePosts)
			}
			if tc.want == "held" && !strings.Contains(string(state.Result), fmt.Sprint(tc.status)) {
				t.Fatal("missing HTTP status")
			}
		})
	}
}

func TestPublishMergeRefusalOwnershipAndIdentity(t *testing.T) {
	for _, name := range []string{"lease lost", "cancelled", "head", "branch", "destination", "repository"} {
		t.Run(name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.MergeFailStatus = 409
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.PullOverride = func(p *gitea.Pull) {
				if f.MergePosts == 0 {
					return
				}
				switch name {
				case "head":
					p.Head.SHA = "different"
				case "branch":
					p.Head.Ref = "different"
				case "destination":
					p.Base.Ref = "different"
				case "repository":
					p.Base.Repo.FullName = "other/repo"
				}
			}
			fence := func(context.Context) error {
				if f.MergePosts > 0 {
					if name == "lease lost" {
						return fmt.Errorf("lease lost")
					}
					if name == "cancelled" {
						cancel()
						return ctx.Err()
					}
				}
				return nil
			}
			_, err := f.Executor.Execute(ctx, f.Job, fence)
			saved, loadErr := f.Executor.Store.Load(f.Job.ID)
			if err == nil || loadErr != nil || saved == nil || hasJSON(saved.FinalState) {
				t.Fatalf("err=%v load=%v", err, loadErr)
			}
		})
	}
}

func TestPublishMergeTransientRefusalRetries(t *testing.T) {
	f := newPipeline(t, "approve")
	f.MergeFailStatus, f.MergeFailCount, f.MergeFailBody = 405, 1, `{"message":"please TRY again later"}`
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || f.MergePosts != 2 || len(f.MergeHeads) != 2 || f.MergeHeads[0] != f.MergeHeads[1] {
		t.Fatalf("state=%+v err=%v posts=%d heads=%v", state, err, f.MergePosts, f.MergeHeads)
	}
}

func TestPublishMergeTransientRefusalLimit(t *testing.T) {
	f := newPipeline(t, "approve")
	f.MergeFailStatus, f.MergeFailBody = 405, `{"message":"Please try again later"}`
	waits := 0
	f.Executor.mergeRetryWait = func(ctx context.Context, delay time.Duration) error {
		waits++
		if delay <= 0 || delay > mergeReadInterval {
			t.Errorf("unexpected retry delay: %v", delay)
		}
		return ctx.Err()
	}
	start := time.Now()
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	elapsed := time.Since(start)
	var view map[string]any
	_ = json.Unmarshal(state.View, &view)
	if err != nil || state.Status != "held" || state.HoldReason != "review_held" || !strings.Contains(string(state.Result), "405") || !strings.Contains(string(state.Result), "Please try again later") || view["releasePending"] != true || f.MergePosts != mergeReadRetries || waits != mergeReadRetries-1 || elapsed > mergeReadLimit+5*time.Second {
		t.Fatalf("state=%+v err=%v view=%v posts=%d elapsed=%v", state, err, view, f.MergePosts, elapsed)
	}
}

func TestPublishMergeTransientRefusalDeadline(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	f.MergeFailStatus, f.MergeFailBody = 405, `{"message":"Please try again later"}`
	f.Executor.mergeRetryWait = func(ctx context.Context, _ time.Duration) error {
		return waitTransient(ctx, mergeReadLimit)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || state.HoldReason != "review_held" || f.MergePosts != 1 || !strings.Contains(string(state.Result), "Please try again later") {
		t.Fatalf("state=%+v err=%v posts=%d", state, err, f.MergePosts)
	}
}

func TestPublishMergeTransientRefusalOwnership(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"lease", "cancel", "head", "branch", "destination"} {
		t.Run(name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.MergeFailStatus, f.MergeFailBody = 405, `{"message":"Please try again later"}`
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads := 0
			f.PullOverride = func(p *gitea.Pull) {
				if f.MergePosts == 0 {
					return
				}
				reads++
				if reads < 2 {
					return
				}
				switch name {
				case "head":
					p.Head.SHA = "different"
				case "branch":
					p.Head.Ref = "different"
				case "destination":
					p.Base.Ref = "different"
				}
			}
			fences := 0
			fence := func(context.Context) error {
				if f.MergePosts > 0 {
					fences++
					if fences >= 2 {
						if name == "lease" {
							return fmt.Errorf("lease lost")
						}
						if name == "cancel" {
							cancel()
							return ctx.Err()
						}
					}
				}
				return nil
			}
			_, err := f.Executor.Execute(ctx, f.Job, fence)
			if f.MergePosts != 1 {
				t.Fatalf("unexpected merge requests: %d", f.MergePosts)
			}
			if name == "lease" || name == "cancel" {
				saved, loadErr := f.Executor.Store.Load(f.Job.ID)
				if err == nil || loadErr != nil || saved == nil || hasJSON(saved.FinalState) {
					t.Fatalf("err=%v load=%v", err, loadErr)
				}
			}
		})
	}
}

func TestPublishMergeTurnApprovalHold(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"ordinary", "承認 final が未了です"},
		{"multiple and scope", "承認 a が未了です；承認 b が未了です、拡大した範囲: a.go, b.go"},
		{"long", "承認 pending " + strings.Repeat("x", 700)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.Executor.secret = "private-value"
			acquire, renew, release := 0, 0, 0
			f.TurnResponse = func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/acquire") {
					acquire++
					_ = json.NewEncoder(w).Encode(map[string]any{"granted": false, "reason": "private-value " + tc.reason})
					return true
				}
				if strings.HasSuffix(r.URL.Path, "/renew") && strings.Contains(r.URL.Path, "merge-turns") {
					renew++
				}
				if strings.HasSuffix(r.URL.Path, "/release") && strings.Contains(r.URL.Path, "merge-turns") {
					release++
				}
				return false
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			var result, view map[string]any
			_ = json.Unmarshal(state.Result, &result)
			_ = json.Unmarshal(state.View, &view)
			saved, loadErr := f.Executor.Store.Load(f.Job.ID)
			summary := fmt.Sprint(result["summary"])
			if err != nil || loadErr != nil || saved == nil || state.Status != "held" || state.HoldReason != "review_held" || acquire != 1 || renew != 0 || release != 0 || f.MergePosts != 0 || view["releasePending"] != true || !strings.Contains(summary, clip(tc.reason, 100)) || !strings.Contains(summary, "[REDACTED]") || len([]rune(summary)) > 530 || strings.Contains(string(saved.FinalState), "private-value") {
				t.Fatalf("state=%+v err=%v load=%v acquire=%d renew=%d release=%d merge=%d", state, err, loadErr, acquire, renew, release, f.MergePosts)
			}
			var stored intake.State
			if json.Unmarshal(saved.FinalState, &stored) != nil || string(stored.Result) != string(state.Result) || string(stored.View) != string(state.View) {
				t.Fatal("hold was not saved")
			}
		})
	}
}

func TestPublishMergeTurnHolderWait(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"granted", "cancel", "lease"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			f := newPipeline(t, "approve")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			acquire := 0
			f.TurnResponse = func(w http.ResponseWriter, r *http.Request) bool {
				if strings.HasSuffix(r.URL.Path, "/acquire") {
					acquire++
					if acquire == 1 {
						_, _ = w.Write([]byte(`{"granted":false,"holder":{"jobId":2}}`))
						if mode == "cancel" {
							cancel()
						}
						return true
					}
				}
				return false
			}
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error {
				if mode == "lease" && acquire > 0 {
					return fmt.Errorf("lease lost")
				}
				return nil
			})
			if mode == "granted" {
				if err != nil || state.Status != "merged" || acquire != 2 || f.MergePosts != 1 {
					t.Fatalf("state=%+v err=%v acquire=%d merge=%d", state, err, acquire, f.MergePosts)
				}
			} else if err == nil || acquire != 1 || f.MergePosts != 0 {
				t.Fatalf("state=%+v err=%v acquire=%d merge=%d", state, err, acquire, f.MergePosts)
			}
		})
	}
}

func TestPublishMergeTurnApprovalHoldResume(t *testing.T) {
	f := newPipeline(t, "approve")
	marker := filepath.Join(f.root, "fixed-runs")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("echo run >> '%s'", marker))
	acquire := 0
	f.TurnResponse = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/acquire") {
			acquire++
			if acquire == 1 {
				_, _ = w.Write([]byte(`{"granted":false,"reason":"approval pending","holder":{"jobId":2}}`))
				return true
			}
		}
		return false
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" {
		t.Fatalf("hold=%+v err=%v", state, err)
	}
	before, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil {
		t.Fatal(err)
	}
	testsBefore, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	calls, created := len(f.Calls), f.CreatePosts
	before.FinalReported = true
	if err := f.Executor.Store.Save(before); err != nil {
		t.Fatal(err)
	}
	state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	after, loadErr := f.Executor.Store.Load(f.Job.ID)
	testsAfter, readErr := os.ReadFile(marker)
	if err != nil || loadErr != nil || readErr != nil || state.Status != "merged" || acquire != 2 || f.MergePosts != 1 || f.CreatePosts != created || len(f.Calls) != calls || string(testsAfter) != string(testsBefore) || after.PR != before.PR || after.Head != before.Head || after.Base != before.Base {
		t.Fatalf("resume=%+v err=%v load=%v read=%v acquire=%d creates=%d calls=%d tests=%q/%q", state, err, loadErr, readErr, acquire, f.CreatePosts, len(f.Calls), testsBefore, testsAfter)
	}
}

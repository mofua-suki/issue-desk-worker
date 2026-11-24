package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/providers"
)

type conflictIntakeProxy struct {
	mu      sync.Mutex
	events  []string
	pending string
	gates   int
	turn    *turnFixture
}

func wrapConflictIntake(t *testing.T, f *pipelineFixture) *conflictIntakeProxy {
	t.Helper()
	turn := setupTurnFixture(t, f)
	turn.block = false
	p := &conflictIntakeProxy{turn: turn}
	original := f.Executor.API
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/merge-turns/") {
			p.mu.Lock()
			p.events = append(p.events, filepath.Base(r.URL.Path))
			p.mu.Unlock()
		}
		if r.URL.Path == "/api/v1/jobs/1" && r.Method == "GET" {
			p.mu.Lock()
			p.gates++
			p.mu.Unlock()
		}
		if r.URL.Path == "/api/v1/commands" {
			p.mu.Lock()
			kind := p.pending
			p.mu.Unlock()
			rows := []command{}
			if kind != "" {
				rows = append(rows, instructionCommand("cmd", kind))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"commands": rows, "next": 1})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/commands/") && strings.HasSuffix(r.URL.Path, "/ack") {
			_, _ = io.WriteString(w, `{}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		response, err := original.Explicit(r.Context(), r.Method, r.URL.RequestURI(), body)
		if err != nil {
			http.Error(w, "proxy error", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
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
func (p *conflictIntakeProxy) assertReleased(t *testing.T) {
	t.Helper()
	p.turn.mu.Lock()
	defer p.turn.mu.Unlock()
	if p.turn.holder != 0 || p.turn.epoch == 0 {
		t.Errorf("repair/review turn owner=%d epoch=%d", p.turn.holder, p.turn.epoch)
	}
}

func (p *conflictIntakeProxy) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.events...)
}
func (p *conflictIntakeProxy) send(kind string) { p.mu.Lock(); defer p.mu.Unlock(); p.pending = kind }

// Advance the actual fixture repository precisely at the first merge POST.
func advanceAtMerge(t *testing.T, f *pipelineFixture) {
	t.Helper()
	h := f.server.Config.Handler
	advanced := false
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !advanced && r.URL.Path == "/api/v1/repos/owner/project/pulls/1/merge" {
			advanced = true
			source := filepath.Join(f.root, "source")
			if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("upstream conflict\n"), 0600); err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			f.git(source, "add", "feature.txt")
			f.git(source, "commit", "-m", "advance base")
			f.git(source, "push", f.bare, "main")
			w.WriteHeader(http.StatusConflict)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func TestMergeConflictTurnReleasedBeforeResolutionAndReacquired(t *testing.T) {
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	f.AllowUsage = true
	p := wrapConflictIntake(t, f)
	advanceAtMerge(t, f)
	handler := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls/1/merge") {
			p.turn.mu.Lock()
			holder := p.turn.holder
			p.turn.mu.Unlock()
			if holder != 1 {
				t.Errorf("merge without owned turn: %d", holder)
			}
		}
		handler.ServeHTTP(w, r)
	})
	marker := filepath.Join(f.root, "fixed-test-passed")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("test -f feature.txt && touch '%s'", marker))
	original := f.Executor.Call
	reviews := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if !req.Review {
			saved, err := f.Executor.Store.Load(1)
			if err != nil || !reflect.DeepEqual(saved.ControllerAllowed, []string{"feature.txt"}) || hasJSON(saved.Test) || hasJSON(saved.Review) {
				t.Fatalf("stale results or conflict scope: %+v, %v", saved, err)
			}
			p.assertReleased(t)
			f.git(req.Policy.Tree, "add", "feature.txt")
			if err := os.WriteFile(filepath.Join(req.Policy.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(req.Policy.Tree, "add", "feature.txt")
		} else {
			if _, err := os.Stat(marker); err != nil {
				t.Error("review ran before fixed test")
			}
			p.assertReleased(t)
			reviews++
		}
		return original(ctx, req)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if reviews != 1 || p.gates < 3 || f.MergePosts != 1 {
		t.Fatalf("revalidation missing: reviews=%d gates=%d merges=%d", reviews, p.gates, f.MergePosts)
	}
	p.assertReleased(t)
	p.turn.mu.Lock()
	defer p.turn.mu.Unlock()
	if p.turn.epoch < 2 {
		t.Fatal("merge did not reacquire after repair")
	}
}

func TestMergeConflictInterruptDuringRetestAndReview(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"test", "review"} {
		for _, kind := range []string{"stop", "instruction"} {
			t.Run(stage+"/"+kind, func(t *testing.T) {
				f := newPipeline(t, "approve")
				prepareMergeRecovery(t, f)
				f.CurrentPolicy = nil
				f.AllowUsage = true
				p := wrapConflictIntake(t, f)
				advanceAtMerge(t, f)
				ready := make(chan struct{}, 1)
				marker := filepath.Join(f.root, "retest-started")
				if stage == "test" {
					f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", fmt.Sprintf("touch '%s'; exec sleep 30", marker)}
				}
				original := f.Executor.Call
				f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
					if !req.Review {
						if err := os.WriteFile(filepath.Join(req.Policy.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
							t.Fatal(err)
						}
						f.git(req.Policy.Tree, "add", "feature.txt")
					} else if stage == "review" {
						ready <- struct{}{}
						<-ctx.Done()
						return providers.Answer{}, ctx.Err()
					}
					return original(ctx, req)
				}
				type outcome struct {
					state intake.State
					err   error
				}
				result := make(chan outcome, 1)
				go func() {
					state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
					result <- outcome{state, err}
				}()
				// The wait only guards against a hang: the fixed test before the review runs
				// real commands and takes longer while the whole suite runs in parallel.
				// Finishing before the stage starts is reported with its state at once.
				deadline := time.After(2 * time.Minute)
				if stage == "test" {
					for started := false; !started; {
						if _, err := os.Stat(marker); err == nil {
							started = true
							continue
						}
						select {
						case done := <-result:
							t.Fatalf("finished before the retest started: state=%+v err=%v", done.state, done.err)
						case <-deadline:
							t.Fatal("retest did not start")
						case <-time.After(20 * time.Millisecond):
						}
					}
				} else {
					select {
					case <-ready:
					case done := <-result:
						t.Fatalf("finished before the review started: state=%+v err=%v", done.state, done.err)
					case <-deadline:
						t.Fatal("review did not start")
					}
				}
				p.assertReleased(t)
				p.send(kind)
				if err := f.Executor.pollCommands(context.Background()); err != nil {
					t.Fatal(err)
				}
				if err := f.Executor.markMergeStarted(1); !errors.Is(err, context.Canceled) {
					t.Fatalf("markMergeStarted: %v", err)
				}
				select {
				case got := <-result:
					want := "stopped"
					if kind == "instruction" {
						want = "queued"
					}
					if got.err != nil || got.state.Status != want || f.merged {
						t.Fatalf("state=%+v err=%v", got.state, got.err)
					}
				case <-time.After(15 * time.Second):
					t.Fatal("execution did not stop")
				}
			})
		}
	}
}

func TestMergeRefusalInterruptDuringCleanFollowRetest(t *testing.T) {
	for _, kind := range []string{"stop", "instruction"} {
		t.Run(kind, func(t *testing.T) {
			f := newPipeline(t, "approve")
			prepareMergeRecovery(t, f)
			f.CurrentPolicy = nil
			p := wrapConflictIntake(t, f)
			handler := f.server.Config.Handler
			attempts := 0
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/pulls/1/merge") {
					attempts++
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
			marker := filepath.Join(f.root, "retest-started")
			f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("touch '%s'; exec sleep 30", marker))
			type outcome struct {
				state intake.State
				err   error
			}
			result := make(chan outcome, 1)
			go func() {
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				result <- outcome{state, err}
			}()
			deadline := time.After(2 * time.Minute)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				select {
				case done := <-result:
					t.Fatalf("finished before retest: state=%+v err=%v", done.state, done.err)
				case <-deadline:
					t.Fatal("retest did not start")
				case <-time.After(20 * time.Millisecond):
				}
			}
			p.send(kind)
			if err := f.Executor.pollCommands(context.Background()); err != nil {
				t.Fatal(err)
			}
			if kind == "instruction" {
				d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1}}
				if err := f.Executor.attachInstructions(&d); err != nil || !strings.Contains(string(d.View["instructions"]), "追加本文") || !strings.Contains(string(d.View["instructions"]), "追加根拠") {
					t.Fatalf("instruction not saved: %s, %v", d.View["instructions"], err)
				}
			}
			select {
			case got := <-result:
				want := "stopped"
				if kind == "instruction" {
					want = "queued"
				}
				if got.err != nil || got.state.Status != want || attempts != 1 || f.merged {
					t.Fatalf("state=%+v err=%v attempts=%d merged=%v", got.state, got.err, attempts, f.merged)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("execution did not stop")
			}
		})
	}
}

func TestMergeFollowNonConflictFailurePropagates(t *testing.T) {
	for _, path := range []string{"after_turn", "after_merge_attempt"} {
		t.Run(path, func(t *testing.T) {
			f := newPipeline(t, "approve")
			prepareMergeRecovery(t, f)
			f.CurrentPolicy = nil
			ctx := context.Background()
			cfg := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
			w, err := gitwork.Open(ctx, cfg, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			record, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			var d Descriptor
			if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
				t.Fatal(err)
			}
			advance := func() {
				source := filepath.Join(f.root, "source")
				if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("upstream\n"), 0600); err != nil {
					t.Fatal(err)
				}
				f.git(source, "add", "feature.txt")
				f.git(source, "commit", "-m", "advance")
				f.git(source, "push", f.bare, "main")
			}
			if path == "after_turn" {
				advance()
			} else {
				advanceAtMerge(t, f)
			}
			// Fail Follow's fetch, after merge's refresh and (when sending a
			// merge) the final snapshot refresh. Keep both failure paths covered.
			failFetch := 2
			if path == "after_merge_attempt" {
				failFetch = 3
			}
			counter := filepath.Join(f.root, "fetch-count")
			script := filepath.Join(f.root, "git-wrapper")
			content := fmt.Sprintf("#!/bin/sh\ncase \" $* \" in *' fetch '*) n=0; if [ -f '%s' ]; then read n < '%s'; fi; n=$((n + 1)); echo \"$n\" > '%s' || exit 1; if [ \"$n\" -eq %d ]; then exit 73; fi;; esac\nexec /usr/bin/git \"$@\"\n", counter, counter, counter, failFetch)
			if err := os.WriteFile(script, []byte(content), 0700); err != nil {
				t.Fatal(err)
			}
			w.Config.Git = script
			p := wrapConflictIntake(t, f)
			merged, changed, err := f.Executor.merge(ctx, d, record, w, func(context.Context) error { return nil })
			if merged || changed || err == nil || errors.Is(err, gitwork.ErrConflict) || !strings.Contains(err.Error(), "73") {
				t.Fatalf("merged=%v changed=%v err=%v", merged, changed, err)
			}
			got := p.snapshot()
			if len(got) < 2 || got[0] != "acquire" || got[len(got)-1] != "release" {
				t.Fatalf("turns %v", got)
			}
		})
	}
}

func TestFullTestFollowConflictRemainsRequestError(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	request := monitorRequest{ID: "123-aabbccdd", Kind: "full_test", Repo: "owner/project", Payload: json.RawMessage(`{}`)}
	w, err := f.Executor.requestWorkspace(ctx, request, "main", "chat/full-test-"+request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Tree, "README.md"), []byte("request branch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.Stage(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit(ctx, "request change"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(f.root, "source")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("new base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "README.md")
	f.git(source, "commit", "-m", "advance main")
	f.git(source, "push", f.bare, "main")
	result, err := f.Executor.fullTestRequest(ctx, request)
	if !errors.Is(err, gitwork.ErrConflict) || result != nil || len(f.Calls) != 0 {
		t.Fatalf("result=%v err=%v calls=%v", result, err, f.Calls)
	}
}

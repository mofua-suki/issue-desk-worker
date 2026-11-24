package execution

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/providers"
)

func TestMergeFollowConflictReturnsToImplementation(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"after_turn", "after_merge_attempt", "stop", "instruction"} {
		t.Run(path, func(t *testing.T) {
			f := newPipeline(t, "approve")
			prepareMergeRecovery(t, f)
			f.CurrentPolicy = nil
			f.AllowUsage = true
			old, _ := f.Executor.Store.Load(1)
			handler := f.server.Config.Handler
			triggered := false
			pullReads := 0
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/repos/owner/project/pulls/1" {
					pullReads++
				}
				match := path == "after_turn" && pullReads == 2 && r.URL.Path == "/api/v1/repos/owner/project/pulls/1" || path != "after_turn" && r.URL.Path == "/api/v1/repos/owner/project/pulls/1/merge"
				if match && !triggered {
					triggered = true
					source := filepath.Join(f.root, "source")
					if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("upstream conflict\n"), 0600); err != nil {
						t.Fatal(err)
					}
					f.git(source, "add", "feature.txt")
					f.git(source, "commit", "-m", "advance base")
					f.git(source, "push", f.bare, "main")
					if path != "after_turn" {
						w.WriteHeader(http.StatusConflict)
						return
					}
				}
				handler.ServeHTTP(w, r)
			})
			previous := f.Executor.Call
			resolved := false
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				if !req.Review {
					resolved = true
					if path == "stop" || path == "instruction" {
						commands, _ := commandExecutor(t, []command{instructionCommand("cmd", path)}, true)
						f.Executor.API = commands.API
						err := f.Executor.pollCommands(context.Background())
						if err != nil || ctx.Err() == nil {
							t.Fatalf("command was not applied: %v", err)
						}
						if err := f.Executor.markMergeStarted(1); err != context.Canceled {
							t.Fatalf("merge restarted after command: %v", err)
						}
						return providers.Answer{}, ctx.Err()
					}
					if strings.Count(req.Prompt, strings.TrimSpace(conflictInstruction)) != 1 {
						t.Errorf("conflict instruction count: %d", strings.Count(req.Prompt, strings.TrimSpace(conflictInstruction)))
					}
					if err := os.WriteFile(filepath.Join(req.Policy.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
						t.Fatal(err)
					}
					f.git(req.Policy.Tree, "add", "feature.txt")
				}
				return previous(ctx, req)
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if path == "stop" || path == "instruction" {
				want := "stopped"
				if path == "instruction" {
					want = "queued"
				}
				if err != nil || state.Status != want || !resolved {
					t.Fatalf("state=%+v err=%v resolved=%v", state, err, resolved)
				}
				return
			}
			if err != nil || state.Status != "merged" || !resolved {
				t.Fatalf("state=%+v err=%v resolved=%v", state, err, resolved)
			}
			after, _ := f.Executor.Store.Load(1)
			if after.Head == old.Head || after.Base == old.Base || len(f.Reviews) < 2 {
				t.Fatal("conflict was not revalidated")
			}
		})
	}
}

func TestMergeConflictResumeFromInterruptedFollow(t *testing.T) {
	f := newPipeline(t, "approve")
	prepareMergeRecovery(t, f)
	f.CurrentPolicy = nil
	f.AllowUsage = true
	ctx := context.Background()
	cfg := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, cfg, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(f.root, "source")
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("upstream conflict\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "feature.txt")
	f.git(source, "commit", "-m", "advance base")
	f.git(source, "push", f.bare, "main")
	if _, err := w.Follow(ctx); !errors.Is(err, gitwork.ErrConflict) {
		t.Fatalf("expected conflict: %v", err)
	}
	previous := f.Executor.Call
	resolved := false
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if !req.Review {
			resolved = true
			f.git(req.Policy.Tree, "add", "feature.txt")
			if err := os.WriteFile(filepath.Join(req.Policy.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(req.Policy.Tree, "add", "feature.txt")
		}
		return previous(ctx, req)
	}
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || !resolved {
		t.Fatalf("state=%+v err=%v resolved=%v", state, err, resolved)
	}
}

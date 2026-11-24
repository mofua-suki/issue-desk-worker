package execution

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/providers"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

func TestResumeTestAndPublishFollowDefaultBeforeFixedTest(t *testing.T) {
	for _, stage := range []string{"test", "publish", "review"} {
		t.Run(stage, func(t *testing.T) {
			f := newPipeline(t, "approve")
			ctx := context.Background()
			c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
			w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			oldBase, err := w.Base(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(w.Tree, "add", "feature.txt")
			f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
			oldHead := f.git(w.Tree, "rev-parse", "HEAD")
			source := filepath.Join(f.root, "source")
			if err := os.WriteFile(filepath.Join(source, "upstream.txt"), []byte("new base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(source, "add", "upstream.txt")
			f.git(source, "commit", "-m", "new base")
			f.git(source, "push", f.bare, "main")
			newBase := f.git(source, "rev-parse", "HEAD")
			f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "test -f feature.txt && test \"$(cat upstream.txt)\" = 'new base'")
			r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: stage, Branch: f.Branch, Provider: "codex", Head: oldHead, Base: oldBase, Test: encode(map[string]any{"passed": true}), Review: encode(map[string]any{"decision": "approve"})}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Base != newBase || saved.Head == oldHead {
				t.Fatalf("base/head: %s %s", saved.Base, saved.Head)
			}
			f.git(w.Tree, "merge-base", "--is-ancestor", newBase, saved.Head)
			if len(f.Calls) != 1 || f.Calls[0] != "claude" {
				t.Fatalf("expected fresh review: %v", f.Calls)
			}
		})
	}
}

func TestResumeTestConflictReturnsToImplementation(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	oldBase, err := w.Base(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Tree, "README.md"), []byte("feature change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(w.Tree, "add", "README.md")
	f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
	oldHead := f.git(w.Tree, "rev-parse", "HEAD")
	source := filepath.Join(f.root, "source")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("upstream change\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "README.md")
	f.git(source, "commit", "-m", "upstream")
	f.git(source, "push", f.bare, "main")
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "test", Branch: f.Branch, Provider: "codex", Head: oldHead, Base: oldBase}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	// Stop before calling the implementation provider; the checkpoint must retain
	// the conflicting file for the next implementation attempt.
	f.Executor.Config.Assignments.ImplementationRunLimit = 0
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.ControllerAllowed) != 1 || saved.ControllerAllowed[0] != "README.md" {
		t.Fatalf("conflicts: %v", saved.ControllerAllowed)
	}
	if len(f.Calls) != 0 {
		t.Fatalf("unexpected provider calls: %v", f.Calls)
	}
}

func TestDirtyResumeConflictReturnsToImplementation(t *testing.T) {
	for _, stage := range []string{"implement", "test", "test_report", "review"} {
		t.Run(stage, func(t *testing.T) {
			f := newPipeline(t, "approve")
			ctx := context.Background()
			c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
			w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			base, err := w.Base(ctx)
			if err != nil {
				t.Fatal(err)
			}
			head, err := w.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Both sides change the same file, but on independent lines.
			if err := os.WriteFile(filepath.Join(w.Tree, "README.md"), []byte("feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(f.root, "source")
			if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("upstream\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(source, "add", "README.md")
			f.git(source, "commit", "-m", "upstream")
			f.git(source, "push", f.bare, "main")
			r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: stage, Branch: f.Branch, Provider: "codex", Head: head, Base: base}
			if stage == "test_report" {
				if err := f.Executor.receiveImplementationReport(r, implementationFixture(), "test_report"); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "true")
			// A real conflict after committing must return to implementation, not fail
			// because merge refused an uncommitted overwrite.
			f.Executor.Config.Assignments.ImplementationRunLimit = 0
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "held" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if len(saved.ControllerAllowed) != 1 || saved.ControllerAllowed[0] != "README.md" {
				t.Fatalf("conflicts=%v", saved.ControllerAllowed)
			}
		})
	}
}

func TestUnchangedPublishReusesTestAndReview(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(w.Tree, "add", "feature.txt")
	f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
	head, err := w.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	base, err := w.Base(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "publish", Branch: f.Branch, Provider: "codex", Head: head, Base: base, Test: encode(map[string]any{"passed": true}), Review: encode(map[string]any{"decision": "approve"})}
	r.Tree = w.Tree
	r.Result = json.RawMessage(`{}`)
	tested, err := f.Executor.fixedTest(ctx, f.Executor.Config.Repositories[0], r, func() error { return nil })
	if err != nil || !tested.Passed {
		t.Fatalf("test=%+v err=%v", tested, err)
	}
	r.Test = encode(tested)
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "exit 1"}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || len(f.Calls) != 0 {
		t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
	}
}

func TestResumeMissingEvidenceRunsTestBeforeReview(t *testing.T) {
	for _, stage := range []string{"held", "review", "publish"} {
		t.Run(stage, func(t *testing.T) {
			f := newPipeline(t, "approve")
			ctx := context.Background()
			c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
			w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(w.Tree, "add", "feature.txt")
			f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
			head, err := w.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			base, err := w.Base(ctx)
			if err != nil {
				t.Fatal(err)
			}
			r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: stage, Branch: f.Branch, Provider: "codex", Head: head, Base: base, Test: encode(map[string]any{"passed": true}), Review: encode(map[string]any{"decision": "approve"})}
			if stage != "held" {
				r.Tree, r.Result = w.Tree, json.RawMessage(`{}`)
				evidence, err := f.Executor.fixedTest(ctx, f.Executor.Config.Repositories[0], r, func() error { return nil })
				if err != nil || !evidence.Passed || evidence.Observation["complete"] != true {
					t.Fatalf("evidence=%+v err=%v", evidence, err)
				}
				if stage == "review" {
					evidence.Observation["sha"] = strings.Repeat("a", 40)
				} else {
					evidence.Observation["complete"] = false
				}
				r.Test = encode(evidence)
			}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || len(f.Calls) != 1 || f.Calls[0] != "claude" {
				t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			var tested TestResult
			if json.Unmarshal(saved.Test, &tested) != nil || testEvidenceReason(tested, head, base) != "" || tested.Observation["complete"] != true {
				t.Fatalf("test=%+v", tested)
			}
		})
	}
}

func TestResumeIncompleteRetestStopsBeforeReviewAndPublish(t *testing.T) {
	for _, stage := range []string{"review", "publish"} {
		t.Run(stage, func(t *testing.T) {
			f := newPipeline(t, "approve")
			ctx := context.Background()
			c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
			w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(w.Tree, "add", "feature.txt")
			f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
			head, err := w.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			base, err := w.Base(ctx)
			if err != nil {
				t.Fatal(err)
			}
			r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: stage, Branch: f.Branch, Provider: "codex", Tree: w.Tree, Head: head, Base: base, Result: json.RawMessage(`{}`), Test: encode(map[string]any{"passed": true}), Review: encode(map[string]any{"decision": "approve"})}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "exit 0"}
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "failed" || len(f.Calls) != 0 {
				t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			var tested TestResult
			if json.Unmarshal(saved.Test, &tested) != nil || tested.Passed || tested.Observation["complete"] == true || saved.FixedTestFailures != 0 {
				t.Fatalf("test=%+v failures=%d", tested, saved.FixedTestFailures)
			}
		})
	}
}

func TestFollowFetchFailurePreservesBaseAndReturnsError(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	base, err := w.Base(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head, err := w.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "test", Branch: f.Branch, Provider: "codex", Head: head, Base: base}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	// Open fetches once; fail the next fetch, inside Follow.
	wrapper := filepath.Join(f.root, "git-wrapper")
	script := "#!/bin/sh\ncase \" $* \" in *' fetch '*) if test -f '" + filepath.Join(f.root, "fetched") + "'; then exit 1; fi; touch '" + filepath.Join(f.root, "fetched") + "';; esac\nexec git \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Git = wrapper
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err == nil || state.Status == "failed" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Base != base || saved.Stage != "test" {
		t.Fatalf("base=%s stage=%s", saved.Base, saved.Stage)
	}
}

func TestDirtyResumeTestsFollowedTree(t *testing.T) {
	for _, stage := range []string{"test", "review"} {
		t.Run(stage, func(t *testing.T) {
			f := newPipeline(t, "approve")
			ctx := context.Background()
			c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
			w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			base, err := w.Base(ctx)
			if err != nil {
				t.Fatal(err)
			}
			head, err := w.Head(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("dirty feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(f.root, "source")
			if err := os.WriteFile(filepath.Join(source, "upstream.txt"), []byte("new base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(source, "add", "upstream.txt")
			f.git(source, "commit", "-m", "upstream")
			f.git(source, "push", f.bare, "main")
			newBase := f.git(source, "rev-parse", "HEAD")
			f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "test \"$(cat feature.txt)\" = 'dirty feature' && test \"$(cat upstream.txt)\" = 'new base'")
			r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: stage, Branch: f.Branch, Provider: "codex", Head: head, Base: base, Test: encode(map[string]any{"passed": true}), Review: encode(map[string]any{"decision": "approve"})}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Base != newBase || saved.Head == head {
				t.Fatalf("base=%s head=%s", saved.Base, saved.Head)
			}
			f.git(w.Tree, "merge-base", "--is-ancestor", newBase, saved.Head)
			if len(f.Calls) != 1 || f.Calls[0] != "claude" {
				t.Fatalf("expected fresh review: %v", f.Calls)
			}
		})
	}
}

// A review sent back in the same run must reach implementation even if the default branch moved.
func TestSendBackInSameRunFollowsDefaultAndKeepsFindings(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	f.Executor.Config.Assignments.ImplementationRunLimit = 20
	f.AllowUsage = true
	const finding = "追従の差し戻しの指摘"
	base := f.Executor.Call
	reviews := 0
	var secondPrompt string
	source := filepath.Join(f.root, "source")
	var newBase string
	implementations := 0
	f.Executor.Call = func(ctx context.Context, request providers.Request) (providers.Answer, error) {
		answer, err := base(ctx, request)
		if err != nil {
			return answer, err
		}
		if !request.Review {
			implementations++
			if implementations == 2 {
				if err := os.WriteFile(filepath.Join(request.Policy.Tree, "revision.txt"), []byte("review repair\n"), 0600); err != nil {
					t.Fatal(err)
				}
				secondPrompt = request.Prompt
				if _, statErr := os.Stat(filepath.Join(request.Policy.Tree, "upstream.txt")); statErr != nil {
					t.Errorf("2回目の実装の前に既定ブランチを取り込んでいません: %v", statErr)
				}
			}
			return answer, nil
		}
		reviews++
		if reviews > 1 {
			return answer, nil
		}
		if err := os.WriteFile(filepath.Join(source, "upstream.txt"), []byte("new base\n"), 0600); err != nil {
			t.Fatal(err)
		}
		f.git(source, "add", "upstream.txt")
		f.git(source, "commit", "-m", "new base")
		f.git(source, "push", f.bare, "main")
		newBase = f.git(source, "rev-parse", "HEAD")
		var review map[string]any
		if err := json.Unmarshal(answer.JSON, &review); err != nil {
			t.Fatal(err)
		}
		review["decision"], review["findings"] = "hold", []string{finding}
		answer.JSON = encode(review)
		return answer, nil
	}
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if implementations != 2 || reviews != 2 {
		t.Fatalf("implementations=%d reviews=%d", implementations, reviews)
	}
	if !strings.Contains(secondPrompt, finding) {
		t.Fatal("差し戻しの指摘が実装の指示に渡っていません")
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Base != newBase {
		t.Fatalf("base=%s want=%s", saved.Base, newBase)
	}
}

func TestResumeWithUnresolvedConflictRepeatsInstruction(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	base, err := w.Base(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Tree, "README.md"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(w.Tree, "add", "README.md")
	f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
	head := f.git(w.Tree, "rev-parse", "HEAD")
	source := filepath.Join(f.root, "source")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("upstream\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "README.md")
	f.git(source, "commit", "-m", "upstream")
	f.git(source, "push", f.bare, "main")
	if changed, followErr := w.Follow(ctx); !changed || !errors.Is(followErr, gitwork.ErrConflict) {
		t.Fatalf("changed=%v err=%v", changed, followErr)
	}
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "implement", Branch: f.Branch, Provider: "codex", Head: head, Base: base}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	var prompts []string
	stub := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, request providers.Request) (providers.Answer, error) {
		if !request.Review {
			prompts = append(prompts, request.Prompt)
			if err := os.WriteFile(filepath.Join(request.Policy.Tree, "README.md"), []byte("resolved\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		return stub(ctx, request)
	}
	if _, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(prompts) == 0 || strings.Count(prompts[0], "既定ブランチとの競合を両側の目的に従って解消してください") != 1 {
		t.Fatalf("競合解消の指示がちょうど1回入っていません: %d", len(prompts))
	}
}

func TestInvalidPendingFilesOnResumeEndsTheJob(t *testing.T) {
	f := newPipeline(t, "approve")
	ctx := context.Background()
	c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable}
	w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
	if err != nil {
		t.Fatal(err)
	}
	base, err := w.Base(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head, err := w.Head(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(w.Tree, "private"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(w.Tree, "private", "value"), []byte("value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "test", Branch: f.Branch, Provider: "codex", Head: head, Base: base}
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestConflictInstructionIsAddedOnce(t *testing.T) {
	body := withConflictInstruction(withConflictInstruction("本文"))
	if strings.Count(body, conflictInstruction) != 1 || !strings.HasPrefix(body, "本文") {
		t.Fatal(body)
	}
}

func TestOrdinaryStagesReuseUnchangedDiff(t *testing.T) {
	for _, tc := range []struct {
		name    string
		advance bool
		final   bool
	}{
		{name: "unchanged"},
		{name: "base changes after turn", advance: true},
		{name: "final approval", final: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *pipelineFixture
			if tc.final {
				f, _ = finalPublicationPipeline(t, "approve")
			} else {
				f = newPipeline(t, "approve")
			}
			log := filepath.Join(f.root, "diff-commands")
			wrapper := filepath.Join(f.root, "git-count")
			script := "#!/bin/sh\ncase \" $* \" in *' diff --name-status -z --no-renames '*) echo names >> '" + log + "';; *' diff --no-ext-diff --no-textconv --no-renames '*) echo body >> '" + log + "';; esac\nexec /usr/bin/git \"$@\"\n"
			if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			f.Executor.Config.Git = wrapper
			if tc.advance {
				advanceOnTurn(t, f)
			}
			marker := filepath.Join(f.root, "test-runs")
			f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "printf x >> '"+marker+"' && test -f feature.txt")
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			data, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			names, body := strings.Count(string(data), "names\n"), strings.Count(string(data), "body\n")
			if names != body || (!tc.advance && !tc.final && names != 1) || (tc.advance && names < 2) || (tc.final && names < 3) {
				t.Fatalf("diff commands: names=%d body=%d", names, body)
			}
			if tc.advance {
				runs, err := os.ReadFile(marker)
				if err != nil || len(runs) < 2 {
					t.Fatalf("retests=%q err=%v", runs, err)
				}
			}
		})
	}
}

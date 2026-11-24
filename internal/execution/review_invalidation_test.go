package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestReviewBaselineInvalidationExecute(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"generation", "instruction", "retry"} {
		t.Run(path, func(t *testing.T) {
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
			snapshot, err := w.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "test", Branch: f.Branch, Provider: "codex", Head: snapshot.Head, Base: snapshot.Base, Result: implementationFixture()}
			review, raw := normalizeReview(encode(map[string]any{"decision": "approve"}))
			if err := os.MkdirAll(filepath.Dir(reviewBaselinePath(c.Root, 1)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := f.Executor.saveReviewBaseline(r, snapshot, review, raw, "full", reviewRequestHash(Descriptor{}, "Implement feature", "受入条件: feature.txtを生成する")); err != nil {
				t.Fatal(err)
			}
			r.Review = raw
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			originalRounds := len(r.ReviewRounds)
			var d Descriptor
			if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
				t.Fatal(err)
			}
			switch path {
			case "generation":
				d.Generation++
				// Match the incoming generation so only the new-generation reset can clear it.
				r.ReviewBaseline.Generation = d.Generation
				if err := f.Executor.Store.Save(r); err != nil {
					t.Fatal(err)
				}
			case "retry":
				d.View = map[string]json.RawMessage{"retryFromBase": json.RawMessage(`{"generation":0}`)}
			case "instruction":
				_, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error {
					control := f.Executor.controls.get(f.Job.ID)
					control.mu.Lock()
					control.reason = "instruction"
					control.mu.Unlock()
					return errors.New("interrupt for instruction")
				})
				if err != nil {
					t.Fatal(err)
				}
				interrupted, err := f.Executor.Store.Load(1)
				if err != nil {
					t.Fatal(err)
				}
				if interrupted.ReviewBaseline != nil || interrupted.Stage != "implement" {
					t.Fatalf("interruption: %+v", interrupted)
				}
				// The runtime reports the queued result before acquiring it again.
				interrupted.FinalReported = true
				if err := f.Executor.Store.Save(interrupted); err != nil {
					t.Fatal(err)
				}
			}
			f.Job.Raw = encode(d)
			calls := 0
			original := f.Executor.Call
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				if req.Review {
					calls++
				}
				return original(ctx, req)
			}
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(saved.ReviewRounds) != originalRounds+1 || saved.ReviewRounds[len(saved.ReviewRounds)-1].Kind != "full" || saved.ReviewSkipped {
				t.Fatalf("calls=%d rounds=%+v skipped=%v", calls, saved.ReviewRounds, saved.ReviewSkipped)
			}
			after, ok := loadReviewBaseline(c.Root, saved)
			if !ok || string(after) != snapshot.Diff {
				t.Fatal("reviewed diff changed")
			}
		})
	}
}

func TestChangedRequestAtMergeKeepsPreviousFindings(t *testing.T) {
	t.Parallel()
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
	s, err := w.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "merge", Branch: f.Branch, Provider: "codex", Head: s.Head, Base: s.Base, Result: implementationFixture(), ReviewSkipped: true}
	fixtureRecordEvidence(t, f, r, w.Tree)
	review, raw := normalizeReview(encode(map[string]any{"decision": "hold", "findings": []string{"以前の重要な指摘"}, "findingKinds": []string{"acceptance"}, "findingSeverities": []string{"must"}}))
	if err := os.MkdirAll(filepath.Dir(reviewBaselinePath(c.Root, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.saveReviewBaseline(r, s, review, raw, "full", reviewRequestHash(Descriptor{}, "Implement feature", "受入条件: feature.txtを生成する")); err != nil {
		t.Fatal(err)
	}
	r.Review = raw
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.View = map[string]json.RawMessage{"instructions": encode([]map[string]string{{"id": "human:1", "text": "追加指示"}})}
	f.Job.Raw = encode(d)
	original := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if req.Review {
			calls++
			if !strings.Contains(req.Prompt, "以前の重要な指摘") {
				t.Error("previous finding missing from review prompt")
			}
		}
		return original(ctx, req)
	}
	_, err = f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || saved.ReviewSkipped || saved.ReviewRounds[len(saved.ReviewRounds)-1].Kind != "full" {
		t.Fatalf("calls=%d skipped=%v rounds=%+v", calls, saved.ReviewSkipped, saved.ReviewRounds)
	}
}

func TestIncomparableReviewReleaseResume(t *testing.T) {
	for _, tc := range []struct {
		name, stage                 string
		unplanned, missing, changed bool
		wantReviews                 int
	}{
		{name: "same planned request", stage: "publish", wantReviews: 1},
		{name: "changed planned request", stage: "publish", changed: true, wantReviews: 2},
		{name: "missing identity at publish", stage: "publish", missing: true, wantReviews: 2},
		{name: "missing identity at merge", stage: "merge", missing: true, wantReviews: 2},
		{name: "missing identity on hold", stage: "held", missing: true, wantReviews: 2},
		{name: "unplanned request", stage: "publish", unplanned: true, wantReviews: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			f.Executor.Config.Repositories[0].AutoMerge = false
			if !tc.unplanned {
				planJob(f, "fixed_test", "implementer", "verification")
			}
			var d Descriptor
			if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
				t.Fatal(err)
			}
			d.Scope = []string{"feature.txt", "asset.bin"}
			f.Job.Raw = encode(d)
			call := f.Executor.Call
			reviews, implementations := 0, 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				if req.Review {
					reviews++
					if len(req.Policy.ReadFiles) != 1 {
						t.Fatal("比較不能な差分のレビューには全差分だけが必要です")
					}
					diff, err := os.ReadFile(req.Policy.ReadFiles[0])
					if err != nil || !strings.Contains(string(diff), "Binary files ") {
						t.Fatalf("バイナリ差分が独立レビューへ届きません: %v", err)
					}
				} else {
					implementations++
					if err := os.WriteFile(filepath.Join(req.Policy.Tree, "asset.bin"), []byte{0, 1, 2}, 0600); err != nil {
						t.Fatal(err)
					}
				}
				return call(ctx, req)
			}
			fence := func(context.Context) error { return nil }
			var goProbe string
			if tc.name == "same planned request" {
				bin := t.TempDir()
				goProbe = filepath.Join(bin, "go-started")
				script := "#!/bin/sh\nprintf 'started\\n' > " + shellArg(goProbe) + "\nprintf '1\\ncc\\n'\n"
				if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			state, err := f.Executor.Execute(t.Context(), f.Job, fence)
			if err != nil || state.Status != "held" || state.PR == nil || reviews != 1 {
				t.Fatalf("first release: status=%s pr=%v reviews=%d err=%v", state.Status, state.PR, reviews, err)
			}
			saved, err := f.Executor.Store.Load(d.ID)
			if err != nil || saved == nil {
				t.Fatal(saved, err)
			}
			saved.Stage, saved.FinalReported = tc.stage, true
			if tc.missing {
				saved.ReviewBaseline = nil
			}
			if err := f.Executor.Store.Save(saved); err != nil {
				t.Fatal(err)
			}
			if tc.changed {
				plan := field[executionPlan](d.View, "executionPlan")
				plan.Requirements[0].Expected = "更新後の期待結果"
				d.View["executionPlan"] = encode(plan)
				d.View["executionPlanSHA"] = encode(strings.Repeat("b", 64))
				f.Job.Raw = encode(d)
			}
			f.Executor.Config.Repositories[0].AutoMerge = true
			state, err = f.Executor.Execute(t.Context(), f.Job, fence)
			if err != nil || state.Status != "merged" || f.MergePosts != 1 || reviews != tc.wantReviews || implementations != 1 {
				t.Fatalf("resume: status=%s merge posts=%d reviews=%d implementations=%d err=%v", state.Status, f.MergePosts, reviews, implementations, err)
			}
			if goProbe != "" {
				if _, err := os.Stat(goProbe); !os.IsNotExist(err) {
					t.Fatalf("能力指定のない固定検証で go を起動しました（マーカーの状態: %v）", err)
				}
			}
		})
	}
}

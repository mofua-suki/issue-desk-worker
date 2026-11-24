package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestConflictUsageResumeAndReviewSendBack(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	f.Executor.Config.Assignments.ImplementationRunLimit = 20
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
	if changed, err := w.Follow(ctx); !changed || !errors.Is(err, gitwork.ErrConflict) {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if err := f.Executor.Store.Save(&journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: "implement", Branch: f.Branch, Provider: "codex", Head: head, Base: base}); err != nil {
		t.Fatal(err)
	}
	stub := f.Executor.Call
	implementations, reviews := 0, 0
	input, output := int64(100), int64(10)
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if !req.Review {
			implementations++
			if implementations == 1 {
				if err := os.WriteFile(filepath.Join(req.Policy.Tree, "README.md"), []byte("resolved\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if implementations == 2 {
				if err := os.WriteFile(filepath.Join(req.Policy.Tree, "README.md"), []byte("resolved and repaired\n"), 0600); err != nil {
					t.Fatal(err)
				}
				saved, err := f.Executor.Store.Load(1)
				if err != nil || len(saved.ControllerAllowed) == 0 {
					t.Fatalf("controller allowed: %+v err=%v", saved, err)
				}
			}
		}
		answer, err := stub(ctx, req)
		answer.Usage = providers.Usage{Input: &input, Output: &output}
		if req.Review {
			reviews++
			if reviews == 1 {
				var review map[string]any
				if err := json.Unmarshal(answer.JSON, &review); err != nil {
					t.Fatal(err)
				}
				review["decision"], review["findings"] = "hold", []string{"修正してください"}
				answer.JSON = encode(review)
			}
		}
		return answer, err
	}
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for i, raw := range saved.Usage {
		var row struct {
			Step               string `json:"step"`
			Generation         int    `json:"generation"`
			DispatchGeneration int    `json:"dispatchGeneration"`
			Input              int64  `json:"inputTokens"`
			Output             int64  `json:"outputTokens"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, row.Step)
		wantGeneration := 1
		if row.Generation != wantGeneration || row.DispatchGeneration != 0 || row.Input != input || row.Output != output {
			t.Errorf("row %d: %+v", i, row)
		}
	}
	if want := []string{"競合解消", "独立レビュー", "実装", "独立レビュー"}; !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps=%v want=%v", steps, want)
	}
	// 差し戻しからの再実装は実装1回として数える。
	if saved.Runs != 2 || saved.ReviewReworkRuns != 1 || saved.WeightedUsage != 600 {
		t.Fatalf("runs=%d weighted=%v", saved.Runs, saved.WeightedUsage)
	}
}

func TestConflictFlagDoesNotChangeReportCompletionStep(t *testing.T) {
	e, r, d, _ := completionFixture(t)
	d.conflictResolution = true
	e.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		return providers.Answer{Session: req.Session, JSON: implementationFixture()}, nil
	}
	state, err := e.completeImplementationReport(context.Background(), d, r, true)
	if err != nil || state != nil {
		t.Fatal(state, err)
	}
	saved, err := e.Store.Load(1)
	if err != nil || len(saved.Usage) != 1 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	var row struct {
		Step string `json:"step"`
	}
	if err := json.Unmarshal(saved.Usage[0], &row); err != nil || row.Step != "実装報告補完" {
		t.Fatalf("row=%+v err=%v", row, err)
	}
}

func TestConflictUsageAntigravityContinuation(t *testing.T) {
	agyHoldFixture(t)
	f := antigravityPipeline(t)
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.conflictResolution = true
	r := &journal.Record{ID: 1, Repo: d.Repo, Issue: d.Issue, Tree: filepath.Join(f.root, "source"), Provider: "antigravity", Runs: 1, Epoch: f.Job.LeaseEpoch}
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		if calls == 1 {
			return agyMeasured(deniedRaw(map[string]any{"tool": "RunCommand", "command": "test"})), nil
		}
		if req.Session != "conv-1" {
			t.Fatalf("session=%q", req.Session)
		}
		return agyMeasured(), nil
	}
	_, err := f.Executor.callAI(context.Background(), d, r, "test", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || calls != 2 || len(saved.Usage) != 2 {
		t.Fatalf("calls=%d saved=%+v err=%v", calls, saved, err)
	}
	for _, raw := range saved.Usage {
		var row struct {
			Step string `json:"step"`
		}
		if err := json.Unmarshal(raw, &row); err != nil || row.Step != "競合解消" {
			t.Fatalf("row=%+v err=%v", row, err)
		}
	}
}

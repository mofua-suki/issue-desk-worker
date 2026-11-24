package execution

import (
	"context"
	"encoding/json"
	"io"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func antigravityPipeline(t *testing.T, admission ...bool) *pipelineFixture {
	f := newPipeline(t, "approve")
	antigravityUsageAPI(t, f, len(admission) == 0 || admission[0])
	if err := json.Unmarshal([]byte(`{"antigravity":{"command":"fixture","settingsFile":"fixture","model":"fixture"}}`), &f.Executor.Config.Fleet); err != nil {
		t.Fatal(err)
	}
	var d Descriptor
	_ = json.Unmarshal(f.Job.Raw, &d)
	d.Provider = "antigravity"
	f.Job.Raw = encode(d)
	return f
}
func TestAntigravityFixedTestFailuresSwitchOnSameTreeAndReport(t *testing.T) {
	f := antigravityPipeline(t)
	original := f.Executor.Call
	tree := ""
	calls := []string{}
	f.Executor.Call = func(ctx context.Context, r providers.Request) (providers.Answer, error) {
		calls = append(calls, r.Settings.Provider)
		if !r.Review {
			if tree != "" && tree != r.Policy.Tree {
				t.Fatal("作業ツリーを引き継いでいません")
			}
			tree = r.Policy.Tree
		}
		if r.Settings.Provider == "antigravity" {
			if err := os.WriteFile(filepath.Join(r.Policy.Tree, "feature.txt"), []byte("needs repair\n"), 0600); err != nil {
				t.Fatal(err)
			}
			return providers.Answer{JSON: implementationFixture(), Session: "agy-1", Outcome: "completed"}, nil
		}
		if !r.Review && r.Settings.Provider == "claude" {
			if r.Session != "" || !strings.Contains(r.Prompt, "固定テスト") || !strings.Contains(r.Prompt, "作業ツリーはそのまま") {
				t.Fatal("会話分離・失敗引継ぎがありません", r.Session, r.Prompt)
			}
		}
		return original(ctx, r)
	}
	out, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || out.Status != "merged" {
		t.Fatal(out, err)
	}
	if strings.Join(calls, ",") != "antigravity,antigravity,claude,codex" {
		t.Fatal(calls)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || !switchedAntigravity(saved) || saved.ProviderSwitch.AppliedAt == "" || len(saved.ProviderSwitch.Failures) != 2 {
		t.Fatal(saved, err)
	}
	notices := 0
	for _, comment := range f.Comments {
		body, _ := comment["body"].(string)
		if strings.Contains(body, "issue-desk-provider-switch:") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatal("切替の通知が1件ではありません", notices)
	}
	if saved.Provider != "claude" || saved.Tree != tree || saved.Runs != 1 {
		t.Fatal("実行先/同tree/回数が違います", saved)
	}
	var view map[string]any
	_ = json.Unmarshal(out.View, &view)
	if view["providerSwitch"] == nil {
		t.Fatal("切替履歴を報告しません", view)
	}
}
func TestAntigravityAutomaticReviewReturnsSwitchAndReviewOpposite(t *testing.T) {
	f := antigravityPipeline(t)
	original := f.Executor.Call
	reviews := 0
	calls := []string{}
	tree := ""
	f.Executor.Call = func(ctx context.Context, r providers.Request) (providers.Answer, error) {
		calls = append(calls, r.Settings.Provider)
		if !r.Review {
			if tree != "" && tree != r.Policy.Tree {
				t.Fatal("別tree")
			}
			tree = r.Policy.Tree
			if err := os.WriteFile(filepath.Join(tree, "marker.txt"), []byte(strings.Join(calls, ",")), 0600); err != nil {
				t.Fatal(err)
			}
		}
		a, err := original(ctx, r)
		if r.Review {
			reviews++
			if reviews <= 2 {
				a.JSON = encode(map[string]any{"decision": "hold", "summary": "修正が必要", "breaking": false, "tooLarge": false, "findings": []string{[]string{"第一の回帰を修正", "第二の回帰を修正"}[reviews-1]}, "findingKinds": []string{"regression"}, "findingSeverities": []string{"blocking"}, "findingSpecs": []any{}, "followups": []string{}, "followupSpecs": []any{}, "improvements": []string{}, "postMergeChecks": []string{}, "userJudgments": []string{}, "resolvableUncertainties": []string{}})
			}
		}
		return a, err
	}
	out, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || out.Status != "merged" {
		t.Fatal(out, err)
	}
	if strings.Join(calls, ",") != "antigravity,codex,antigravity,codex,claude,codex" {
		t.Fatal(calls)
	}
	r, _ := f.Executor.Store.Load(1)
	if !switchedAntigravity(r) || r.ProviderSwitch.Failures[0].Kind != "review" || r.ReviewHolds != 2 {
		t.Fatal(r)
	}
}
func TestAntigravitySwitchSurvivesRestartAndHumanInstructionsResetOnlyStrikes(t *testing.T) {
	root := t.TempDir()
	store, err := journal.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{Store: store}
	r := &journal.Record{ID: 1, Repo: "owner/repo", Issue: 1, Provider: "antigravity"}
	d := Descriptor{Descriptor: jobspec.Descriptor{Provider: "antigravity", View: map[string]json.RawMessage{}}}
	if err = e.syncAntigravityInputs(d, r); err != nil {
		t.Fatal(err)
	}
	if _, err = e.noteAntigravityFailure(context.Background(), d, r, "test", "one"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	store, err = journal.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e.Store = store
	r, err = store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	d.View["instructions"] = encode([]map[string]string{{"id": "review:1", "text": "自動差戻し"}})
	_ = e.syncAntigravityInputs(d, r)
	if len(r.AntigravityStrikes) != 1 {
		t.Fatal("自動差戻しが失敗数を消した")
	}
	d.View["instructions"] = encode([]map[string]string{{"id": "human:1", "text": "新しい指示"}})
	_ = e.syncAntigravityInputs(d, r)
	if len(r.AntigravityStrikes) != 0 {
		t.Fatal("本人指示で数え直さない")
	}
	_, _ = e.noteAntigravityFailure(context.Background(), d, r, "test", "one")
	switched, err := e.noteAntigravityFailure(context.Background(), d, r, "review", "two")
	if err != nil || !switched {
		t.Fatal(err)
	}
	d.View["instructions"] = encode([]map[string]string{{"id": "human:2", "text": "次の指示"}})
	_ = e.syncAntigravityInputs(d, r)
	if len(r.AntigravityStrikes) != 0 || implementationProvider(d, r) != "claude" {
		t.Fatal("切替まで消した", r)
	}
	saved, err := store.Load(1)
	if err != nil || !switchedAntigravity(saved) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Provider = "antigravity"
	if _, err = e.noteAntigravityFailure(ctx, d, r, "test", "cancelled"); err == nil {
		t.Fatal("取消後に進めた")
	}
}

// Reimplementation admission is an actual Server event, including for the in-turn provider switch.
func antigravityUsageAPI(t *testing.T, f *pipelineFixture, required bool) {
	original := f.API
	checks := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/jobs/1/events" {
			var body struct {
				Epoch int64  `json:"epoch"`
				Kind  string `json:"kind"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Epoch != 3 || body.Kind != "implementation_usage_check" {
				t.Error("使用量門の実eventが違います")
			}
			checks++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"job":{"id":1},"usageLimitReason":null}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		reply, err := original.Explicit(r.Context(), r.Method, r.URL.RequestURI(), raw)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.Status)
		w.Write(reply.Body)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() {
		if required && checks == 0 {
			t.Error("使用量門を通りませんでした")
		}
	})
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	f.API = api
	f.Executor.API = api
}

func TestAntigravityUnavailableFixedTestDoesNotCountOrSwitch(t *testing.T) {
	f := antigravityPipeline(t)
	f.Executor.Config.Repositories[0].TestCommand = []string{"/missing-fixture-test"}
	original := f.Executor.Call
	calls := []string{}
	f.Executor.Call = func(ctx context.Context, r providers.Request) (providers.Answer, error) {
		calls = append(calls, r.Settings.Provider)
		return original(ctx, r)
	}
	out, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || out.Status != "failed" {
		t.Fatal(out, err)
	}
	saved, _ := f.Executor.Store.Load(1)
	if len(saved.AntigravityStrikes) != 0 || saved.ProviderSwitch != nil || strings.Join(calls, ",") != "antigravity,antigravity,antigravity" {
		t.Fatal("実行不能を不合格2回に数えました", calls, saved)
	}
}
func TestAntigravityCancelledSwitchDoesNotCallClaudeAndResumes(t *testing.T) {
	f := antigravityPipeline(t, false)
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Provider: "antigravity", Tree: filepath.Join(f.Executor.Config.RuntimeDir, "workspaces", "resume"), Epoch: 3}
	if err := os.MkdirAll(r.Tree, 0700); err != nil {
		t.Fatal(err)
	}
	d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Provider: "antigravity"}}
	_, _ = f.Executor.noteAntigravityFailure(context.Background(), d, r, "test", "one")
	_, _ = f.Executor.noteAntigravityFailure(context.Background(), d, r, "test", "two")
	r, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	original := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		if req.Settings.Provider != "claude" || req.Session != "" {
			t.Fatal(req.Settings.Provider, req.Session)
		}
		return original(ctx, req)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = f.Executor.callAI(ctx, d, r, "resume", false, nil); err == nil || calls != 0 {
		t.Fatal("取消後にClaudeを開始", err, calls)
	}
	if _, err = f.Executor.callAI(context.Background(), d, r, "resume", false, nil); err != nil || calls != 1 {
		t.Fatal("保存switchを再開できません", err, calls)
	}
}

func TestAntigravitySizeOnlyReviewDoesNotCountAsFailureStrike(t *testing.T) {
	f := antigravityPipeline(t)
	original := f.Executor.Call
	turn, reviews := 0, 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		a, err := original(ctx, req)
		if !req.Review {
			turn++
			_ = os.WriteFile(filepath.Join(req.Policy.Tree, "marker.txt"), []byte(strings.Repeat("x", turn)), 0600)
		} else if reviews++; reviews <= 2 {
			var review map[string]any
			_ = json.Unmarshal(a.JSON, &review)
			review["decision"] = "hold"
			review["tooLarge"] = true
			a.JSON = encode(review)
		}
		return a, err
	}
	out, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || out.Status != "merged" || reviews != 3 {
		t.Fatal(out, err)
	}
	r, _ := f.Executor.Store.Load(1)
	if r.ProviderSwitch != nil || len(r.AntigravityStrikes) > 0 {
		t.Fatal("大きさだけの保留を失敗strikeに数えました", r)
	}
}

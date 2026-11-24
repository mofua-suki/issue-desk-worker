package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/jobspec"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"issue-desk-worker/internal/testcause"
)

func runCountDescriptor(generation int, view map[string]any) Descriptor {
	d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Generation: generation, View: map[string]json.RawMessage{}}}
	for key, value := range view {
		d.View[key] = encode(value)
	}
	return d
}

func setGeneration(t *testing.T, f *pipelineFixture, generation int, view map[string]any) {
	t.Helper()
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.Generation = generation
	for key, value := range view {
		d.View[key] = encode(value)
	}
	f.Job.Raw, f.CurrentView = encode(d), d.View
}

// 結果の報告が受付に届いた後の状態にする。Execute が保存済みの結果を返さず、次の lease として動く。
func acknowledgeFinal(t *testing.T, f *pipelineFixture) {
	t.Helper()
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	saved.FinalReported = true
	if err := f.Executor.Store.Save(saved); err != nil {
		t.Fatal(err)
	}
}

func askingCall(f *pipelineFixture, calls *int) {
	base := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		*calls++
		answer, err := base(ctx, req)
		answer.JSON = reportWith("status", "question")
		return answer, err
	}
}

func execute(f *pipelineFixture) (intake.State, error) {
	return f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
}

type historySaveFailureTransport struct {
	next    http.RoundTripper
	store   *journal.Store
	badRoot string
}

func (tr *historySaveFailureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := tr.next.RoundTrip(req)
	if err == nil && strings.HasSuffix(req.URL.Path, "/state") {
		tr.store.Root = tr.badRoot
	}
	return res, err
}

func TestImplementationDoesNotStartWhenHistorySaveFails(t *testing.T) {
	f := newPipeline(t, "approve")
	base := completionAPI(t, f)
	originalRoot := f.Executor.Store.Root
	defer func() { f.Executor.Store.Root = originalRoot }()
	tr := &historySaveFailureTransport{next: base, store: f.Executor.Store, badRoot: filepath.Join(f.root, "missing")}
	api, err := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), tr)
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.API = api
	_, err = execute(f)
	if err == nil || len(f.Calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, f.Calls)
	}
}

func TestContinuationExclusionRestoredWhenImplementationStarts(t *testing.T) {
	for _, keepOther := range []bool{false, true} {
		t.Run(fmt.Sprintf("other exclusion=%t", keepOther), func(t *testing.T) {
			f := newPipeline(t, "approve")
			completionAPI(t, f)
			f.Executor.Config.Assignments.ImplementationRunLimit = 4
			w := continuationWorkspace(t, f)
			head := f.git(w.Tree, "rev-parse", "HEAD")
			d := continuationDescriptor(t, f, head)
			d.Generation = 1
			r := &journal.Record{ID: 1, Repo: d.Repo, Issue: d.Issue, Generation: 1, Epoch: 2, Stage: "implement", Branch: f.Branch, Head: head, Runs: 3, ReportCompletionRuns: 1, UnansweredRuns: 1, ExcludedGenerations: []int{1}, ContinuationExcludedGenerations: []int{1}, StartHistory: &journal.StartHistory{Generation: 1, Epoch: 2, Known: true, Host: "fixture"}}
			if keepOther {
				r.OtherExcludedGenerations = []int{1}
			}
			d.View["workerExecution"] = encode(workerExecutionView(r, "implement"))
			f.Job.Raw = encode(d)
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			calls := 0
			askingCall(f, &calls)
			state, err := execute(f)
			if err != nil || calls != 1 || state.HoldReason != "question" {
				t.Fatalf("state=%+v calls=%d err=%v", state, calls, err)
			}
			got, err := f.Executor.Store.Load(1)
			wantRuns := 4
			if keepOther {
				wantRuns = 3
			}
			if err != nil || got.Runs != wantRuns || slices.Contains(got.ExcludedGenerations, 1) != keepOther || !slices.Contains(got.StartedGenerations, 1) || len(got.ContinuationExcludedGenerations) != 0 {
				t.Fatalf("record=%+v err=%v", got, err)
			}
		})
	}
}

func TestImplementationRunCountMatchesIntakeFormula(t *testing.T) {
	for _, tc := range []struct {
		name       string
		generation int
		view       map[string]any
		want       int
	}{
		{name: "first", want: 1},
		{name: "generations", generation: 3, want: 4},
		{name: "resumes", generation: 3, view: map[string]any{"implementationResumes": 2}, want: 2},
		{name: "each-list", generation: 3, view: map[string]any{"retestPassedGenerations": []int{1}, "prerequisiteMissingGenerations": []int{2}, "codexNotStartedGenerations": []int{3}}, want: 1},
		{name: "same-generation-in-two-lists", generation: 3, view: map[string]any{"retestPassedGenerations": []int{2}, "codexNotStartedGenerations": []int{2}}, want: 3},
		{name: "intake-keeps-retest-of-generation-zero", generation: 1, view: map[string]any{"retestPassedGenerations": []int{0}}, want: 2},
		{name: "future-generation-is-ignored", generation: 1, view: map[string]any{"codexNotStartedGenerations": []int{9}}, want: 2},
		{name: "never-below-one", generation: 1, view: map[string]any{"implementationResumes": 5}, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := implementationRunCount(runCountDescriptor(tc.generation, tc.view), &journal.Record{}); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// 世代3の時点で、世代1は環境の失敗。世代2は受付の再開で、重なるときは世代2も環境の失敗にする。
func TestExclusionAndResumeCountSurviveSaveAndReload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		excluded []int
		view     map[string]any
		want     int
	}{
		{name: "not-overlapping", excluded: []int{1}, want: 2},
		// 世代2は再開として既に引かれているので、除外で二重に引かない。
		{name: "resume-overlaps-exclusion", excluded: []int{1, 2}, want: 2},
		// 受付の一覧と同じ世代も1回だけ引く。
		{name: "intake-list-overlaps-exclusion", excluded: []int{1}, view: map[string]any{"codexNotStartedGenerations": []int{1}}, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := journal.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			view := map[string]any{"implementationResumes": 1}
			for key, value := range tc.view {
				view[key] = value
			}
			r := &journal.Record{ID: 1, Generation: 3, Stage: "implement", Runs: 9, ExcludedGenerations: tc.excluded, ResumedGenerations: []int{2}, ResumesSeen: 1}
			d := runCountDescriptor(3, view)
			if got := syncImplementationRuns(d, r, startedExecution{}, false); got != tc.want || r.Runs != tc.want {
				t.Fatalf("got %d runs %d, want %d", got, r.Runs, tc.want)
			}
			if err := store.Save(r); err != nil {
				t.Fatal(err)
			}
			loaded, err := store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			if got := syncImplementationRuns(d, loaded, startedExecution{}, false); got != tc.want || loaded.Runs != tc.want {
				t.Fatalf("after reload got %d runs %d, want %d", got, loaded.Runs, tc.want)
			}
		})
	}
}

// journal の無い別のPCでも、checkpoint の記録から同じ回数になる。
func TestExclusionAndResumeSurviveMoveToAnotherPC(t *testing.T) {
	r := &journal.Record{Generation: 2, ExcludedGenerations: []int{1, 2}, ResumedGenerations: []int{2}, ResumesSeen: 1, Runs: 2}
	var started startedExecution
	if err := json.Unmarshal(encode(workerExecutionView(r, "implement")), &started); err != nil {
		t.Fatal(err)
	}
	moved := &journal.Record{Generation: 3}
	d := runCountDescriptor(3, map[string]any{"implementationResumes": 1})
	if got := syncImplementationRuns(d, moved, started, true); got != 2 || moved.Runs != 1 {
		t.Fatalf("got %d runs %d", got, moved.Runs)
	}
	if !slices.Equal(moved.ExcludedGenerations, []int{1, 2}) || !slices.Equal(moved.ResumedGenerations, []int{2}) {
		t.Fatal(moved)
	}
}

func TestResumeIncreaseMarksCurrentGeneration(t *testing.T) {
	r := &journal.Record{}
	syncImplementationRuns(runCountDescriptor(2, nil), r, startedExecution{}, true)
	syncImplementationRuns(runCountDescriptor(3, map[string]any{"implementationResumes": 1}), r, startedExecution{}, true)
	syncImplementationRuns(runCountDescriptor(4, map[string]any{"implementationResumes": 1}), r, startedExecution{}, true)
	if !slices.Equal(r.ResumedGenerations, []int{3}) || r.ResumesSeen != 1 {
		t.Fatal(r.ResumedGenerations, r.ResumesSeen)
	}
}

func TestSameGenerationResumedFiveTimesDoesNotReachRunLimit(t *testing.T) {
	f := newPipeline(t, "approve")
	tr := completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 1
	calls := 0
	askingCall(f, &calls)
	for i := 1; i <= 5; i++ {
		state, err := execute(f)
		if err != nil || state.HoldReason != "question" || strings.Contains(string(state.View), "implementationRunLimit") {
			t.Fatal(i, state, err)
		}
		if calls != i {
			t.Fatalf("resume %d made %d calls", i, calls)
		}
		saved, err := f.Executor.Store.Load(1)
		if err != nil || saved.Runs != 1 {
			t.Fatal(i, saved, err)
		}
		acknowledgeFinal(t, f)
	}
	// 1回目は未開始の世代。2回目以降は開始済みの世代なので使用量を確かめる。
	if len(tr.checks) != 4 {
		t.Fatalf("restarts must check usage: %d", len(tr.checks))
	}
}

func TestGenerationOverRunLimitStops(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 2
	calls := 0
	askingCall(f, &calls)
	for generation := 0; generation <= 1; generation++ {
		setGeneration(t, f, generation, nil)
		state, err := execute(f)
		if err != nil || state.HoldReason != "question" || strings.Contains(string(state.View), "implementationRunLimit") || calls != generation+1 {
			t.Fatal(generation, state, err, calls)
		}
		acknowledgeFinal(t, f)
	}
	setGeneration(t, f, 2, nil)
	state, err := execute(f)
	if err != nil || !strings.Contains(string(state.View), `"implementationRunLimit":true`) || calls != 2 {
		t.Fatal(state, err, calls)
	}
	// 上限超えの承認は今までどおり上限を広げる。
	acknowledgeFinal(t, f)
	setGeneration(t, f, 3, map[string]any{"overLimitApprovals": []map[string]any{{"id": "a", "statement": "承認", "implementationRuns": 3}}})
	state, err = execute(f)
	if err != nil || strings.Contains(string(state.View), "implementationRunLimit") || calls != 3 {
		t.Fatal(state, err, calls)
	}
}

// 開始ごとに増えた旧 journal の Runs は同じ式へ計算し直し、上限超えの承認を求めない。
func TestLegacyInflatedRunsAreRecomputed(t *testing.T) {
	f := newPipeline(t, "approve")
	publishContinuationFixture(t, f)
	completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 4
	calls := 0
	askingCall(f, &calls)
	if err := f.Executor.Store.Save(&journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Generation: 2, Stage: "held", Branch: f.Branch, Provider: "codex", Runs: 5, FinalReported: true}); err != nil {
		t.Fatal(err)
	}
	setGeneration(t, f, 3, nil)
	state, err := execute(f)
	if err != nil || strings.Contains(string(state.View), "implementationRunLimit") || calls != 1 {
		t.Fatal(state, err, calls)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.Runs != 4 {
		t.Fatal(saved, err)
	}
}

func TestEnvironmentFailureIsNotCounted(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 1
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "echo \"EACCES: permission denied, lstat '/outside/cache'\" >&2; exit 1"}
	state, err := execute(f)
	if err != nil || state.Status != "failed" {
		t.Fatal(state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || !slices.Equal(saved.ExcludedGenerations, []int{0}) {
		t.Fatal(saved, err)
	}
	acknowledgeFinal(t, f)
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "true")
	setGeneration(t, f, 1, nil)
	state, err = execute(f)
	if err != nil || state.Status != "merged" {
		t.Fatal("環境の失敗で上限に達してはいけません", state, err)
	}
	if saved, err = f.Executor.Store.Load(1); err != nil || saved.Runs != 1 {
		t.Fatal(saved, err)
	}
}

func TestUndeterminedFixedTestFailureIsStillCounted(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 1
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "exit 1"}
	state, err := execute(f)
	if err != nil || state.Status != "failed" {
		t.Fatal(state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || len(saved.ExcludedGenerations) != 0 {
		t.Fatal(saved, err)
	}
	acknowledgeFinal(t, f)
	setGeneration(t, f, 1, nil)
	state, err = execute(f)
	if err != nil || !strings.Contains(string(state.View), `"implementationRunLimit":true`) {
		t.Fatal(state, err)
	}
}

// 実際の固定テストの証拠を基点の証拠として保存し、同じ失敗の世代が除外されることを確かめる。
func TestBaselineFailureIsNotCounted(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("..", "testcause", "testdata", "issue22-go-fail.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 1
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "cat '" + fixture + "'; exit 1"}
	f.Executor.Config.Repositories[0].TestTimeoutSeconds = 30
	state, err := execute(f)
	if err != nil || state.Status != "failed" {
		t.Fatal(state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || len(saved.ExcludedGenerations) != 0 {
		t.Fatal("基点の証拠が無い失敗は数えます", saved, err)
	}
	var tested TestResult
	if err := json.Unmarshal(saved.Test, &tested); err != nil || tested.Observation["complete"] != true {
		t.Fatal(string(saved.Test), err)
	}
	baseline := map[string]any{}
	for key, value := range tested.Observation {
		baseline[key] = value
	}
	baseline["sha"], baseline["source"] = tested.Observation["baseSha"], "full-test"
	file := filepath.Join(f.Executor.Config.RuntimeDir, "control", "test-observations", "owner__project.json")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, encode([]any{baseline}), 0600); err != nil {
		t.Fatal(err)
	}
	acknowledgeFinal(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 2
	setGeneration(t, f, 1, nil)
	state, err = execute(f)
	if err != nil || state.Status != "failed" {
		t.Fatal(state, err)
	}
	if saved, err = f.Executor.Store.Load(1); err != nil || !slices.Equal(saved.ExcludedGenerations, []int{1}) || saved.Runs != 1 {
		t.Fatal("基点でも同じ失敗は数えません", saved, err)
	}
}

func TestFixedTestNotAttributableNeedsMatchingBaseline(t *testing.T) {
	cfg := Configuration{}
	cfg.RuntimeDir = t.TempDir()
	e := &Executor{Config: cfg}
	output, err := os.ReadFile(filepath.Join("..", "testcause", "testdata", "issue22-go-fail.txt"))
	if err != nil {
		t.Fatal(err)
	}
	m := testcause.Measurement{SHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Source: "fixed-test", Command: []string{"go", "test", "./..."}, Environment: map[string]any{"go": "go1.24", "tz": "UTC"}, Output: string(output), Code: 1, Started: time.Unix(1, 0), Finished: time.Unix(2, 0), CompletedFiles: 1}
	r := &journal.Record{Repo: "owner/project"}
	if e.fixedTestNotAttributable(r, TestResult{Observation: testcause.Create(m)}) {
		t.Fatal("基点の証拠が無いのに除外しました")
	}
	if e.fixedTestNotAttributable(r, TestResult{}) {
		t.Fatal("証拠の無い失敗を除外しました")
	}
	if !e.fixedTestNotAttributable(r, TestResult{Unavailable: true}) {
		t.Fatal("実行できなかったテストは除外します")
	}
	if e.fixedTestNotAttributable(r, TestResult{Passed: true, Unavailable: true}) {
		t.Fatal("合格は失敗ではありません")
	}
}

// 回数で止まらない同じ世代の繰り返しは、使用量の上限で止まる。
func TestSameGenerationResumeStopsAtUsageLimit(t *testing.T) {
	f := newPipeline(t, "approve")
	completionAPI(t, f)
	f.Executor.Config.Assignments.ImplementationRunLimit = 4
	calls := 0
	askingCall(f, &calls)
	for i := 1; i <= 3; i++ {
		if state, err := execute(f); err != nil || state.HoldReason != "question" {
			t.Fatal(state, err)
		}
		acknowledgeFinal(t, f)
	}
	transport := &usageDecisionTransport{next: f.API}
	api, err := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), transport)
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.API = api
	state, err := execute(f)
	if err != nil || !strings.Contains(string(state.View), `"implementationUsageStop":true`) || strings.Contains(string(state.View), "implementationRunLimit") {
		t.Fatal(state, err)
	}
	if calls != 3 || transport.checks != 1 {
		t.Fatalf("使用量の上限の後に実装しました: calls=%d checks=%d", calls, transport.checks)
	}
}

// 回答だけで変更の無い世代も、受付が再開と判定しなければ1世代として数える。
func TestAnswerOnlyGenerationCountsUnlessIntakeResumes(t *testing.T) {
	r := &journal.Record{}
	if got := syncImplementationRuns(runCountDescriptor(1, nil), r, startedExecution{}, true); got != 2 {
		t.Fatal(got)
	}
	if got := syncImplementationRuns(runCountDescriptor(1, map[string]any{"implementationResumes": 1}), r, startedExecution{}, true); got != 1 {
		t.Fatal(got)
	}
}

// 差し戻しからの再実装の回数は checkpoint で別のPCへ渡り、同じ世代の総回数に残る。
func TestReviewReworkRunsSurviveMoveToAnotherPC(t *testing.T) {
	r := &journal.Record{Runs: 4, ReviewReworkRuns: 3}
	var started startedExecution
	if err := json.Unmarshal(encode(workerExecutionView(r, "implement")), &started); err != nil {
		t.Fatal(err)
	}
	moved := &journal.Record{}
	carryStartedRuns(moved, started)
	d := runCountDescriptor(0, nil)
	got := syncImplementationRuns(d, moved, started, false)
	if got != 1 || moved.Runs != 4 || moved.ReviewReworkRuns != 3 || !generationStarted(moved, got) {
		t.Fatalf("got %d record %+v", got, moved)
	}
}

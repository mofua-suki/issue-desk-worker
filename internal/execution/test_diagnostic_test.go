package execution

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestTimeoutDiagnosticSurvivesJournalAndFinalResult(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	completionAPI(t, f)
	f.Executor.Config.TestTimeoutSeconds = 1
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "printf 'stage=database start=fixture\nidk_secret123\n'; sleep 3"}
	state, err := f.Executor.Execute(t.Context(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" {
		t.Fatal(state, err)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	var tested TestResult
	if json.Unmarshal(saved.Test, &tested) != nil || tested.Diagnostic == nil {
		t.Fatal("中断の診断がありません")
	}
	d := tested.Diagnostic
	if d.Reason != "timeout" || !d.StartedProcess || d.Termination == "" || d.UserMillis == nil || d.Head == nil || d.Base == "" || len(d.Stages) == 0 || d.Timeout != 1 {
		t.Fatalf("中断の根拠不足: %+v", d)
	}
	if strings.Contains(string(saved.Test), "idk_secret123") {
		t.Fatal("資格情報を保存")
	}
	var result map[string]json.RawMessage
	_ = json.Unmarshal(state.Result, &result)
	if len(result["fixedTestDiagnostic"]) == 0 {
		t.Fatal("受付への引継ぎがありません")
	}
}

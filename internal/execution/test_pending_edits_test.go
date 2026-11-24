package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/jobspec"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

func pendingFixture(t *testing.T) (*pipelineFixture, *journal.Record, *gitwork.Workspace, Repository) {
	t.Helper()
	f := newPipeline(t, "approve")
	tree := filepath.Join(f.root, "edits")
	if err := os.Mkdir(tree, 0700); err != nil {
		t.Fatal(err)
	}
	f.git(tree, "init")
	f.git(tree, "config", "user.name", "Fixture")
	f.git(tree, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(tree, "remove.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "check.test.js"), []byte("const {test}=require('node:test');const fs=require('node:fs');const assert=require('node:assert/strict');test('removed',()=>assert.equal(fs.existsSync('remove.txt'),false));\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(tree, "add", ".")
	f.git(tree, "commit", "-m", "base")
	head := f.git(tree, "rev-parse", "HEAD")
	r := &journal.Record{ID: 1, Epoch: 3, Repo: "owner/project", Issue: 7, Tree: tree, Base: head, Head: head, Provider: "codex", Stage: "test"}
	f.Executor.Config.Host = "a9max"
	w := &gitwork.Workspace{Tree: tree, Config: gitwork.Config{Git: f.Executor.Config.Git, User: "Fixture"}}
	return f, r, w, Repository{TestCommand: []string{f.Executor.Config.Node, "--test", "check.test.js"}}
}

func TestFinalApprovalTestHoldNamesPendingEdit(t *testing.T) {
	f, r, _, repo := pendingFixture(t)
	report := json.RawMessage(`{"status":"completed","deleteFiles":["remove.txt"],"addDependencies":[{"name":"example-pkg","version":"1"}]}`)
	if err := f.Executor.receiveImplementationReport(r, report, "test"); err != nil {
		t.Fatal(err)
	}
	d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1}, finalApprovalFixedTest: true}
	state, err := f.Executor.implementationTests(context.Background(), d, repo, r, "")
	if err != nil || state == nil || state.Status != "held" || state.HoldReason != "review_held" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	var result struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(state.Result, &result); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"deleteFiles", "remove.txt", "addDependencies", "example-pkg", r.EditRequestedAt.UTC().Format(time.RFC3339), "a9max", "不確か"} {
		if !strings.Contains(result.Summary, value) {
			t.Fatalf("missing %s: %s", value, result.Summary)
		}
	}
	if _, err := os.Stat(filepath.Join(r.Tree, "remove.txt")); err != nil {
		t.Fatal("pending deletion executed", err)
	}
	var test TestResult
	if json.Unmarshal(r.Test, &test) != nil || test.Observation != nil {
		t.Fatal("fixed test ran")
	}
	r.Result = json.RawMessage(`null`)
	test, err = f.Executor.fixedTest(context.WithValue(context.Background(), finalApprovalTestKey{}, true), repo, r)
	if err != nil || test.Passed || !test.evidenceMissing || test.Observation != nil {
		t.Fatalf("missing report: %+v %v", test, err)
	}
}

func appliedFixture(t *testing.T) (*pipelineFixture, *journal.Record, Repository) {
	t.Helper()
	f, r, w, repo := pendingFixture(t)
	var report map[string]any
	if err := json.Unmarshal(implementationFixture(), &report); err != nil {
		t.Fatal(err)
	}
	report["deleteFiles"] = []string{"remove.txt"}
	if err := f.Executor.receiveImplementationReport(r, encode(report), "test"); err != nil {
		t.Fatal(err)
	}
	prepare := f.Executor.testTargetPreparer(context.Background(), w, r, "apply", func(context.Context) error { return nil })
	test, err := f.Executor.fixedTest(context.Background(), repo, r, prepare)
	if err != nil || !test.Passed {
		t.Fatalf("initial test %+v: %v", test, err)
	}
	r.Test = encode(test)
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	return f, r, repo
}

func TestFinalApprovalTestAppliedEditsNotPending(t *testing.T) {
	f, r, repo := appliedFixture(t)
	head := r.Head
	d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, HeadSHA: head}, finalApprovalFixedTest: true}
	state, err := f.Executor.implementationTests(context.Background(), d, repo, r, "")
	var result TestResult
	decodeErr := json.Unmarshal(r.Test, &result)
	if err != nil || decodeErr != nil || state != nil || !result.Passed || d.HeadSHA != head || r.Head != head || f.git(r.Tree, "rev-parse", "HEAD") != head {
		t.Fatalf("state=%+v result=%+v err=%v decode=%v", state, result, err, decodeErr)
	}
	r.AppliedEdits.Head = strings.Repeat("a", 40)
	state, err = f.Executor.implementationTests(context.Background(), d, repo, r, "")
	if err != nil || state == nil || state.Status != "held" || state.HoldReason != "review_held" || d.HeadSHA != head || r.Head != head {
		t.Fatalf("mismatch=%+v err=%v", state, err)
	}
}

func TestFinalApprovalTestAfterWorkerMove(t *testing.T) {
	f, original, repo := appliedFixture(t)
	head := original.Head
	if err := f.Executor.checkpoint(context.Background(), Descriptor{Descriptor: jobspec.Descriptor{ID: 1}}, original, "merge"); err != nil {
		t.Fatal(err)
	}
	if len(f.ReceivedStates) != 1 || f.ReceivedStates[0].Status != "running" || f.ReceivedStates[0].HeadSHA != head {
		t.Fatalf("checkpoint states=%+v", f.ReceivedStates)
	}
	delivered := Descriptor{Descriptor: jobspec.Descriptor{HeadSHA: head, Result: f.ReceivedStates[0].Result}}
	moved := &journal.Record{ID: 1, Epoch: 2, Repo: original.Repo, Tree: original.Tree, Head: original.Head, Base: original.Base}
	f.Executor.Config.Host = "nab-9s"
	seedFinalPublicationReport(delivered, moved)
	if len(moved.Result) == 0 {
		t.Fatal("delivered report not restored")
	}
	f.Executor.seedFinalPublicationTest(context.Background(), delivered, moved)
	result, err := f.Executor.fixedTest(context.WithValue(context.Background(), finalApprovalTestKey{}, true), repo, moved)
	if err != nil || !result.Passed || moved.AppliedEdits == nil || moved.Head != head || f.git(moved.Tree, "rev-parse", "HEAD") != head {
		t.Fatalf("moved=%+v err=%v", result, err)
	}
	for _, scenario := range []string{"missing report", "different head", "unapplied edits"} {
		t.Run(scenario, func(t *testing.T) {
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(delivered.Result, &payload); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "missing report":
				delete(payload, "implementationResult")
			case "different head":
				payload["implementationContent"] = encode(strings.Repeat("a", 40))
			case "unapplied edits":
				delete(payload, "fixedTest")
			}
			d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, HeadSHA: head, Result: encode(payload)}}
			r := &journal.Record{ID: 1, Epoch: original.Epoch, Repo: original.Repo, Tree: original.Tree, Head: head, Base: original.Base}
			seedFinalPublicationReport(d, r)
			f.Executor.seedFinalPublicationTest(context.Background(), d, r)
			state, err := f.Executor.implementationTests(context.Background(), Descriptor{Descriptor: d.Descriptor, finalApprovalFixedTest: true}, repo, r, "")
			if err != nil || state == nil || state.Status != "held" || r.AppliedEdits != nil || r.Head != head {
				t.Fatalf("state=%+v applied=%+v err=%v", state, r.AppliedEdits, err)
			}
		})
	}
}

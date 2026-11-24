package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func finalApprovalFixture(t *testing.T) (*Executor, *journal.Record, Descriptor, func(...string) string) {
	t.Helper()
	e, r, d, _ := completionFixture(t)
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = r.Tree
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		b, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", b, err)
		}
		return strings.TrimSpace(string(b))
	}
	git("init")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(r.Tree, "sample.test.js"), []byte("const {test}=require('node:test'); test('ok',()=>{});\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "fixture")
	r.Head = git("rev-parse", "HEAD")
	r.Base = r.Head
	r.Stage = "test"
	r.Result = json.RawMessage(`{"status":"completed"}`)
	sandbox := filepath.Join(t.TempDir(), "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = -- ]; then shift; exec \"$@\"; fi; shift; done\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e.Config.Codex = sandbox
	d.finalApprovalFixedTest = true
	return e, r, d, git
}

func TestFinalApprovalFixedTestPassPreservesHead(t *testing.T) {
	e, r, d, git := finalApprovalFixture(t)
	marker := filepath.Join(t.TempDir(), "runs")
	program := "const {test}=require('node:test'); const fs=require('node:fs'); test('ok',()=>fs.appendFileSync(" + strconv.Quote(marker) + ", 'x'));\n"
	if err := os.WriteFile(filepath.Join(r.Tree, "sample.test.js"), []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "count executions")
	r.Head = git("rev-parse", "HEAD")
	head := r.Head
	r.FixedTestFailures = 2
	calls := 0
	prepares := 0
	e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		calls++
		return providers.Answer{}, nil
	}
	state, err := e.implementationTests(context.Background(), d, Repository{TestCommand: []string{e.Config.Node, "--test", "sample.test.js"}}, r, "", func() error { prepares++; return nil })
	var result TestResult
	runs, _ := os.ReadFile(marker)
	if string(runs) != "x" || err != nil || state != nil || json.Unmarshal(r.Test, &result) != nil || !result.Passed || testEvidenceReason(result, head, r.Base) != "" || r.FixedTestFailures != 0 || calls != 0 || prepares != 0 || git("rev-parse", "HEAD") != head || git("status", "--porcelain") != "" {
		t.Fatalf("state=%+v err=%v result=%+v calls=%d prepares=%d", state, err, result, calls, prepares)
	}
}

func TestFinalApprovalFixedTestFailureHoldsWithoutAI(t *testing.T) {
	for _, command := range [][]string{{"/bin/sh", "-c", "echo failed; exit 1"}, {"/missing-test-command"}, {"/bin/true"}} {
		t.Run(command[0]+strings.Join(command[1:], " "), func(t *testing.T) {
			e, r, d, git := finalApprovalFixture(t)
			head := r.Head
			calls, prepares := 0, 0
			e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				calls++
				return providers.Answer{}, nil
			}
			state, err := e.implementationTests(context.Background(), d, Repository{TestCommand: command}, r, "", func() error { prepares++; return nil })
			if err != nil || state == nil || state.Status != "held" || state.HoldReason != "review_held" || calls != 0 || prepares != 0 || git("rev-parse", "HEAD") != head || !strings.Contains(string(state.Result), "最終承認") {
				t.Fatalf("state=%+v err=%v calls=%d prepares=%d", state, err, calls, prepares)
			}
		})
	}
}
func TestFinalApprovalFixedTestMutationHolds(t *testing.T) {
	for _, dirty := range []bool{true, false} {
		t.Run(strconv.FormatBool(dirty), func(t *testing.T) {
			e, r, d, git := finalApprovalFixture(t)
			unformatted := []byte("package fixture\nfunc Example( ){println(1)}\n")
			if err := os.WriteFile(filepath.Join(r.Tree, "format.go"), unformatted, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(r.Tree, "go.mod"), []byte("module fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			git("add", ".")
			git("commit", "-m", "unformatted input")
			r.Head = git("rev-parse", "HEAD")
			head := r.Head
			calls, prepares := 0, 0
			e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				calls++
				return providers.Answer{}, nil
			}
			if dirty {
				if err := os.WriteFile(filepath.Join(r.Tree, "dirty"), []byte("dirty"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := []string{"/bin/sh", "-c", "echo changed > mutation; exit 0"}
			state, err := e.implementationTests(context.Background(), d, Repository{TestCommand: cmd}, r, "", func() error { prepares++; return nil })
			var result TestResult
			_ = json.Unmarshal(r.Test, &result)
			formatted, _ := os.ReadFile(filepath.Join(r.Tree, "format.go"))
			if string(formatted) != string(unformatted) || err != nil || state == nil || state.Status != "held" || state.HoldReason != "review_held" || result.Passed || calls != 0 || prepares != 0 || git("rev-parse", "HEAD") != head {
				t.Fatalf("state=%+v err=%v test=%+v calls=%d prepares=%d", state, err, result, calls, prepares)
			}
		})
	}
}
func TestFinalApprovalFixedTestReportStageDoesNotCallAI(t *testing.T) {
	e, r, d, _ := finalApprovalFixture(t)
	r.Stage = "test_report"
	calls := 0
	e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		calls++
		return providers.Answer{}, nil
	}
	state, err := e.implementationTests(context.Background(), d, Repository{TestCommand: []string{"/bin/true"}}, r, "")
	if err != nil || state == nil || state.Status != "held" || state.HoldReason != "review_held" || calls != 0 {
		t.Fatal(state, err, calls)
	}
}
func TestFinalApprovalReportReadyRunsFixedTest(t *testing.T) {
	e, r, d, git := finalApprovalFixture(t)
	r.Stage = "test_report"
	r.Result = implementationFixture()
	head := r.Head
	calls := 0
	e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		calls++
		return providers.Answer{}, nil
	}
	state, err := e.implementationTests(context.Background(), d, Repository{TestCommand: []string{e.Config.Node, "--test", "sample.test.js"}}, r, "")
	var result TestResult
	_ = json.Unmarshal(r.Test, &result)
	if err != nil || state != nil || !result.Passed || testEvidenceReason(result, head, r.Base) != "" || calls != 0 || git("rev-parse", "HEAD") != head {
		t.Fatal(state, err, result, calls)
	}
}

func TestFinalApprovalPendingEditsHold(t *testing.T) {
	e, r, d, _ := finalApprovalFixture(t)
	r.Result = json.RawMessage(`{"status":"completed","deleteFiles":["sample.test.js"]}`)
	state, err := e.implementationTests(context.Background(), d, Repository{TestCommand: []string{e.Config.Node, "--test", "sample.test.js"}}, r, "")
	if err != nil || state == nil || state.Status != "held" || state.HoldReason != "review_held" {
		t.Fatal(state, err)
	}
	if _, err := os.Stat(filepath.Join(r.Tree, "sample.test.js")); err != nil {
		t.Fatal(err)
	}
}

func TestFinalApprovalClaudeStagedEditsHold(t *testing.T) {
	e, r, d, git := finalApprovalFixture(t)
	r.Provider = "claude"
	// Ignored staging files do not make the Git working tree dirty.
	if err := os.WriteFile(filepath.Join(r.Tree, ".gitignore"), []byte(".claude-staged/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitignore")
	git("commit", "-m", "ignore staging")
	r.Head = git("rev-parse", "HEAD")
	stage := filepath.Join(r.Tree, ".claude-staged", "hooks", "check")
	writeFixture(t, stage, "pending")
	marker := filepath.Join(t.TempDir(), "ran")
	state, err := e.implementationTests(context.Background(), d, Repository{TestCommand: []string{"/bin/sh", "-c", "touch " + marker}}, r, "")
	var result TestResult
	_ = json.Unmarshal(r.Test, &result)
	if err != nil || state == nil || state.Status != "held" || state.HoldReason != "review_held" || !strings.Contains(result.Detail, "Claude") {
		t.Fatalf("state=%+v err=%v result=%+v", state, err, result)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("test command ran: %v", err)
	}
	if data, err := os.ReadFile(stage); err != nil || string(data) != "pending" {
		t.Fatalf("staged edit changed: %q %v", data, err)
	}
}

func TestFixedTestDefaultStillRepairs(t *testing.T) {
	e, r, d, _ := finalApprovalFixture(t)
	d.finalApprovalFixedTest = false
	calls := 0
	e.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		calls++
		return providers.Answer{}, errors.New("fixture stop")
	}
	_, err := e.implementationTests(context.Background(), d, Repository{TestCommand: []string{"/bin/false"}}, r, "")
	if err != nil || calls != 1 || r.FixedTestFailures != 1 {
		t.Fatal(err, calls, r.FixedTestFailures)
	}
}

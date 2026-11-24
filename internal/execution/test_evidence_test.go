package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"issue-desk-worker/internal/testcause"
)

func TestMixedOutputEvidenceSurvivesClassification(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	output := "# tests 397\n# pass 397\n# fail 0\n# cancelled 0\n# skipped 0\nok  example.com/project  0.012s\n"
	for _, warning := range []string{"[WARN] Issue while reading \"/home/node/.npmrc\". EACCES\n", ""} {
		m := testcause.Measurement{SHA: head, BaseSHA: base, Source: "fixed-test", Command: []string{"node", "--test"}, Environment: testcause.Object{"node": "v24", "tz": "UTC"}, Output: warning + output, Code: 0, Started: time.Unix(100, 0), Finished: time.Unix(101, 0), CompletedFiles: 397}
		o := testcause.Create(m)
		raw, err := json.Marshal(TestResult{Passed: true, Observation: o})
		var recovered TestResult
		if err != nil || json.Unmarshal(raw, &recovered) != nil || recovered.Observation["complete"] != true || testEvidenceReason(recovered, head, base) != "" {
			t.Fatalf("warning=%q observation=%#v", warning, o)
		}
		m.Code, m.Output, m.Source = 0, warning+output, "full-test"
		if got := testcause.Create(m); got["complete"] != true || testcause.Classify(got, nil)["target"] == nil {
			t.Fatalf("full-test observation lost: %#v", got)
		}
	}
	m := testcause.Measurement{SHA: head, BaseSHA: base, Source: "fixed-test", Command: []string{"node", "--test"}, Environment: testcause.Object{"node": "v24", "tz": "UTC"}, Output: "# tests 1\n# pass 0\n# fail 1\n# cancelled 0\n", Code: 1, Started: time.Unix(100, 0), Finished: time.Unix(101, 0), CompletedFiles: 397}
	if got := testcause.Create(m); got["complete"] == true {
		t.Fatalf("failure summary accepted as complete: %#v", got)
	}
}

func TestSkippedOutputEvidenceAndMissingConditions(t *testing.T) {
	head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
	m := testcause.Measurement{SHA: head, BaseSHA: base, Source: "fixed-test", Command: []string{"node", "--test", "sample.test.js"}, Environment: testcause.Object{"node": "v24", "tz": "UTC"}, Output: "ℹ tests 1577\nℹ pass 1576\nℹ fail 0\nℹ cancelled 0\nℹ skipped 1\n", Code: 0, Started: time.Unix(100, 0), Finished: time.Unix(101, 0), CompletedFiles: 1}
	check := func(measurement testcause.Measurement) string {
		t.Helper()
		raw, err := json.Marshal(TestResult{Passed: true, Observation: testcause.Create(measurement)})
		var recovered TestResult
		if err != nil || json.Unmarshal(raw, &recovered) != nil {
			t.Fatal("evidence serialization failed")
		}
		return testEvidenceReason(recovered, head, base)
	}
	if reason := check(m); reason != "" {
		t.Fatalf("valid skipped run rejected: %s", reason)
	}
	cases := []struct {
		name, want string
		change     func(*testcause.Measurement)
	}{
		{"completion", "完了件数", func(v *testcause.Measurement) { v.CompletedFiles = 0 }},
		{"conditions", "実行条件", func(v *testcause.Measurement) { v.Environment = testcause.Object{"tz": "UTC"} }},
		{"target", "対象SHA", func(v *testcause.Measurement) { v.SHA = "" }},
		{"base", "基点SHA", func(v *testcause.Measurement) { v.BaseSHA = "" }},
		{"output", "出力の完全性", func(v *testcause.Measurement) { v.Truncated = true }},
		{"format", "保存形式の検証", func(v *testcause.Measurement) { v.Started = v.Finished.Add(time.Second) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := m
			tc.change(&v)
			if reason := check(v); !strings.Contains(reason, tc.want) {
				t.Fatalf("reason %q, want %q", reason, tc.want)
			}
		})
	}
}

func TestFixedEvidencePersistsThroughReviewAndReport(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	if err := os.Mkdir(tree, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = tree
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(tree, "sample.test.js"), []byte("const {test} = require('node:test'); const assert = require('node:assert/strict'); const fs = require('node:fs'); test('feature', () => assert.equal(fs.readFileSync('feature', 'utf8'), 'change'));\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "package.json"), []byte(`{"name":"evidence-fixture","version":"1.0.0","scripts":{"test":"node --test sample.test.js"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, ".gitignore"), []byte("node_modules/\npnpm-lock.yaml\npnpm-workspace.yaml\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "base")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(tree, "feature"), []byte("change"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module fixture\n\ngo 1.23\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "format.go"), []byte("package fixture\nfunc Example( ){println(1)}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "format_test.go"), []byte("package fixture\nimport (\"testing\"; \"os\")\nfunc TestFixture(t *testing.T) {}\nfunc TestRequiredCheck(t *testing.T) { b, err := os.ReadFile(\"feature\"); if err != nil || string(b) != \"change\" { t.Fatal(err, string(b)) } }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "more_test.go"), []byte("package fixture\nimport \"testing\"\nfunc TestOther(t *testing.T) {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(root, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = -- ]; then shift; exec \"$@\"; fi; shift; done\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(root, "runtime")
	store, err := journal.Open(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: runtimeDir, Git: "/usr/bin/git", GitUser: "Fixture", Codex: sandbox, Node: node, TestTimeoutSeconds: 300}}, Store: store}
	r := &journal.Record{ID: 1, Epoch: 1, Repo: "owner/project", Tree: tree, Base: base, Result: json.RawMessage(`{"status":"completed","acceptanceTransfers":[],"followupIssues":[]}`)}
	w := &gitwork.Workspace{Tree: tree, Config: gitwork.Config{Git: "/usr/bin/git", User: "Fixture"}}
	prepare := e.testTargetPreparer(context.Background(), w, r, "feature", func(context.Context) error { return nil })
	test, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{"go", "test", "./..."}}, r, prepare)
	if err != nil || !test.Passed {
		t.Fatalf("test=%+v err=%v", test, err)
	}
	directProof := test.Observation["passEvidence"].(map[string]any)["record"].(map[string]any)
	if directProof["files"] != float64(1) {
		t.Fatalf("go package count: %#v", directProof)
	}
	formatted, err := os.ReadFile(filepath.Join(tree, "format.go"))
	if err != nil || !strings.Contains(string(formatted), "func Example()") || git("status", "--porcelain") != "" {
		t.Fatalf("format was not committed: %s, %v", formatted, err)
	}
	r.Test = encode(test)
	if err := store.Save(r); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if reason := e.reviewTestEvidence(context.Background(), saved, saved.Head); reason != "" {
		t.Fatal(reason)
	}
	prompt := reviewPrompt(Descriptor{}, "", "", saved, gitwork.Snapshot{Head: saved.Head}, "")
	const marker = "根拠(データ): "
	var review map[string]json.RawMessage
	if i := strings.LastIndex(prompt, marker); i < 0 || json.Unmarshal([]byte(prompt[i+len(marker):]), &review) != nil {
		t.Fatal("review data missing")
	}
	var reviewed TestResult
	if err := json.Unmarshal(review["test"], &reviewed); err != nil || testEvidenceReason(reviewed, saved.Head) != "" || reviewed.Observation["complete"] != true || reviewed.Observation["sha"] != saved.Head {
		t.Fatalf("review: %+v %v", reviewed, err)
	}
	state, err := e.finish(saved, "failed", "", "fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Cause struct {
			Target map[string]any `json:"target"`
		} `json:"fixedTestCause"`
	}
	if err := json.Unmarshal(state.Result, &result); err != nil || result.Cause.Target["sha"] != saved.Head || result.Cause.Target["complete"] != true {
		t.Fatalf("result=%s err=%v", state.Result, err)
	}
	// An actual package-manager run remains usable, but its file count is unknown.
	pnpm := testPNPM(t)
	wrapped, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{pnpm, "test"}}, saved, e.testTargetPreparer(context.Background(), w, saved, "feature", func(context.Context) error { return nil }))
	if err != nil || wrapped.Passed || !wrapped.evidenceMissing || wrapped.Observation["complete"] != false {
		t.Fatalf("wrapped=%+v err=%v", wrapped, err)
	}
	// A wrapper script (like scripts/verify.sh) proves its pass through the tree's Go
	// packages it reports as ok; an ok line for a package outside the tree proves nothing.
	script, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{"/bin/sh", "-c", "go test ./... && printf '# tests 1\\n# pass 1\\n# fail 0\\n'"}}, saved, e.testTargetPreparer(context.Background(), w, saved, "feature", func(context.Context) error { return nil }))
	if err != nil || !script.Passed || script.Observation["complete"] != true || testEvidenceReason(script, saved.Head) != "" {
		t.Fatalf("script=%+v err=%v", script, err)
	}
	// The Go result precedes enough output to fall outside the stored detail excerpt.
	// This is a representative wrapper output; the incident's original logs were unavailable.
	long, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{"/bin/sh", "-c", "go test -v ./... && printf '# tests 1\\n# pass 1\\n# fail 0\\n' && yes 'successful output' | head -n 1000"}}, saved, e.testTargetPreparer(context.Background(), w, saved, "feature", func(context.Context) error { return nil }))
	if err != nil || !long.Passed || strings.Contains(long.Detail, "ok  ") {
		t.Fatalf("long output: %+v err=%v", long, err)
	}
	saved.Test = encode(long)
	if err := store.Save(saved); err != nil {
		t.Fatal(err)
	}
	saved, err = store.Load(1)
	if err != nil || e.reviewTestEvidence(context.Background(), saved, saved.Head) != "" {
		t.Fatalf("long output review: %v", err)
	}
	prompt = reviewPrompt(Descriptor{}, "", "", saved, gitwork.Snapshot{Head: saved.Head}, "")
	if i := strings.LastIndex(prompt, marker); i < 0 || json.Unmarshal([]byte(prompt[i+len(marker):]), &review) != nil || json.Unmarshal(review["test"], &reviewed) != nil {
		t.Fatal("long output review data missing")
	}
	target, _ := testcause.Classify(reviewed.Observation, nil)["target"].(map[string]any)
	proof, _ := target["passEvidence"].(map[string]any)
	record, _ := proof["record"].(map[string]any)
	excerpt, _ := record["excerpt"].(string)
	if target["complete"] != true || target["sha"] != saved.Head || record["sha"] != saved.Head || record["files"] != float64(1) || !strings.Contains(excerpt, "--- PASS: TestRequiredCheck") {
		t.Fatalf("long output proof: %#v", target)
	}
	// A bare node --test proves its pass through the tree's default-pattern test files
	// and the runner's complete summary.
	bare, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{node, "--test"}}, saved, e.testTargetPreparer(context.Background(), w, saved, "feature", func(context.Context) error { return nil }))
	if err != nil || !bare.Passed || bare.Observation["complete"] != true {
		t.Fatalf("bare node --test: %+v err=%v", bare, err)
	}
	if record, _ := bare.Observation["passEvidence"].(map[string]any)["record"].(map[string]any); record["files"] != 1 && record["files"] != float64(1) {
		t.Fatalf("bare node --test files: %#v", record)
	}
	// The configured command may exclude named tests (sample-scraper's command in
	// the intake's fleet.json). A failing test it matches is left out of the run.
	// It runs on a copy of the record and the tree is reset afterwards, so the checks
	// below keep their head.
	beforeExclusion := strings.TrimSpace(git("rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(tree, "excluded.test.js"), []byte("require('node:test')('configured exclusion', () => { throw new Error('excluded') })\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	exclusionRecord := *saved
	skipping, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{node, "--test", "--test-skip-pattern=^configured exclusion$"}}, &exclusionRecord, e.testTargetPreparer(context.Background(), w, &exclusionRecord, "feature", func(context.Context) error { return nil }))
	git("reset", "--hard", beforeExclusion)
	if err != nil || !skipping.Passed || skipping.Observation["complete"] != true {
		t.Fatalf("node --test with a skip pattern: %+v err=%v", skipping, err)
	}
	if record, _ := skipping.Observation["passEvidence"].(map[string]any)["record"].(map[string]any); record["files"] != 2 && record["files"] != float64(2) {
		t.Fatalf("node --test with a skip pattern files: %#v", record)
	}
	// A runner summary without evidence tying completed files to the tree is insufficient.
	for name, command := range map[string][]string{
		"node narrowing the run": {node, "--test", "--test-only"},
		"pytest wrapper summary": {"/bin/sh", "-c", "printf 'test session starts\\n============ 1 passed in 0.01s ============\\n'"},
	} {
		t.Run(name, func(t *testing.T) {
			result, runErr := e.fixedTest(context.Background(), Repository{TestCommand: command}, saved, e.testTargetPreparer(context.Background(), w, saved, "feature", func(context.Context) error { return nil }))
			if runErr != nil || result.Passed || result.Observation["complete"] == true || result.Observation["passEvidence"] != nil {
				t.Fatalf("unsupported wrapper accepted: %+v err=%v", result, runErr)
			}
		})
	}
	forged, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{"/bin/sh", "-c", "printf 'ok  \\tfixture/missing\\t0.1s\\n# pass 1\\n# fail 0\\n'"}}, saved, e.testTargetPreparer(context.Background(), w, saved, "feature", func(context.Context) error { return nil }))
	if err != nil || forged.Passed || forged.Observation["complete"] != false {
		t.Fatalf("forged=%+v err=%v", forged, err)
	}
	saved.Test = encode(wrapped)
	if err := store.Save(saved); err != nil {
		t.Fatal(err)
	}
	saved, err = store.Load(1)
	if err != nil || e.reviewTestEvidence(context.Background(), saved, saved.Head) == "" {
		t.Fatalf("wrapper review: %v", err)
	}
	wrappedState, err := e.finish(saved, "failed", "", "fixture", nil)
	var wrappedReport struct {
		Fixed TestResult `json:"fixedTest"`
		Cause struct {
			Target map[string]any `json:"target"`
		} `json:"fixedTestCause"`
	}
	if err != nil || json.Unmarshal(wrappedState.Result, &wrappedReport) != nil || wrappedReport.Cause.Target["complete"] != false || wrappedReport.Cause.Target["sha"] != saved.Head {
		t.Fatalf("wrapper report=%s err=%v", wrappedState.Result, err)
	}
	for _, tc := range []struct {
		name string
		sha  any
		head string
	}{{"null", nil, saved.Head}, {"invalid", "bad", saved.Head}, {"different observation", strings.Repeat("a", 40), saved.Head}, {"other head", saved.Head, strings.Repeat("a", 40)}} {
		t.Run(tc.name, func(t *testing.T) {
			var altered TestResult
			_ = json.Unmarshal(encode(test), &altered)
			altered.Observation["sha"] = tc.sha
			saved.Test = encode(altered)
			if e.reviewTestEvidence(context.Background(), saved, tc.head) == "" {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	saved.Test = encode(test)
	if err := os.WriteFile(filepath.Join(tree, "sample.test.js"), []byte("const {test} = require('node:test'); const fs = require('node:fs'); test('changes tree', () => fs.writeFileSync('feature', 'changed'));\n"), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{node, "--test", "sample.test.js"}}, saved, e.testTargetPreparer(context.Background(), w, saved, "feature", func(context.Context) error { return nil }))
	if err != nil || changed.Passed || !changed.evidenceMissing || changed.Observation["passed"] == true {
		t.Fatalf("changed target accepted: %+v err=%v", changed, err)
	}
	if err := os.WriteFile(filepath.Join(tree, "feature"), []byte("changed during test"), 0600); err != nil {
		t.Fatal(err)
	}
	if reason := e.reviewTestEvidence(context.Background(), saved, saved.Head); reason == "" {
		t.Fatal("changed tree accepted")
	}
	git("reset", "--hard")
	committingTest := "const {test} = require('node:test'); const fs = require('node:fs'); const {execFileSync} = require('node:child_process'); test('changes head', () => { fs.writeFileSync('feature','new head'); execFileSync('git',['add','feature']); execFileSync('git',['commit','-m','changed']); });\n"
	if err := os.WriteFile(filepath.Join(tree, "sample.test.js"), []byte(committingTest), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err = e.fixedTest(context.Background(), Repository{TestCommand: []string{node, "--test", "sample.test.js"}}, saved, e.testTargetPreparer(context.Background(), w, saved, "feature", func(context.Context) error { return nil }))
	if err != nil || changed.Passed || !changed.evidenceMissing || changed.Observation["passed"] == true {
		t.Fatalf("changed head accepted: %+v err=%v", changed, err)
	}
}

func TestFullTestRequestBrowserTools(t *testing.T) {
	for _, tc := range []struct {
		name, repo                                        string
		tools, probeOK, wantProbe, wantTools, wantWarning bool
	}{
		{"intake", "mofua-suki/issue-desk-intake", true, true, true, true, false},
		{"other", "owner/project", true, true, false, false, false},
		{"missing-tools", "mofua-suki/issue-desk-intake", false, true, false, false, false},
		{"probe-failure", "mofua-suki/issue-desk-intake", true, false, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			if tc.repo != "owner/project" {
				f.Executor.Config.Repositories[0].Name = tc.repo
				f.GiteaOverride = func(_ http.ResponseWriter, r *http.Request) bool {
					r.URL.Path = strings.Replace(r.URL.Path, "/api/v1/repos/mofua-suki/issue-desk-intake", "/api/v1/repos/owner/project", 1)
					return false
				}
				repoDir := filepath.Join(f.root, "repositories", "mofua-suki")
				if err := os.MkdirAll(repoDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(f.bare, filepath.Join(repoDir, "issue-desk-intake.git")); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			caddy, chromium, codex, probe := filepath.Join(dir, "caddy"), filepath.Join(dir, "chromium"), filepath.Join(dir, "codex"), filepath.Join(dir, "probe")
			if tc.tools {
				for _, path := range []string{caddy, chromium} {
					if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			code := "1"
			if tc.probeOK {
				code = "0"
			}
			script := fmt.Sprintf("#!/bin/sh\ncase $* in *--dump-dom*) : > %q; exit %s;; esac\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -- ]; then shift; exec \"$@\"; fi; shift; done\nexit 8\n", probe, code)
			if err := os.WriteFile(codex, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			f.Executor.Config.Codex = codex
			f.Executor.browserTools = func() (string, string) { return caddy, chromium }
			wantCaddy, wantChromium := "unset", "unset"
			if tc.wantTools {
				wantCaddy, wantChromium = caddy, chromium
			}
			check := fmt.Sprintf(`test "${ISSUE_DESK_TEST_CADDY-unset}" = %q && test "${ISSUE_DESK_TEST_CHROMIUM-unset}" = %q`, wantCaddy, wantChromium)
			f.Executor.Config.Repositories[0].TestCommand = []string{f.Executor.Config.Node, "fixture-command.test.js", check}
			head := f.git(f.root, "--git-dir", f.bare, "rev-parse", "main")
			result, err := f.Executor.fullTestRequest(context.Background(), monitorRequest{ID: "123-aabbccdd", Kind: "full_test", Repo: tc.repo, Payload: json.RawMessage(`{}`)})
			if err != nil || result["status"] != "passed" || result["sha"] != head {
				t.Fatalf("result=%v err=%v", result, err)
			}
			_, probeErr := os.Stat(probe)
			if (probeErr == nil) != tc.wantProbe || (probeErr != nil && !os.IsNotExist(probeErr)) {
				t.Fatalf("probe error=%v", probeErr)
			}
			if strings.Contains(fmt.Sprint(result["excerpt"]), chromiumProbeWarning) != tc.wantWarning {
				t.Fatalf("excerpt=%v", result["excerpt"])
			}
		})
	}
}

func TestFullTestRequestPersistsCompleteBaseObservation(t *testing.T) {
	f := newPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "feature.txt")
	f.git(source, "commit", "-m", "feature")
	f.git(source, "push", f.bare, "main")
	head := f.git(source, "rev-parse", "HEAD")
	result, err := f.Executor.fullTestRequest(context.Background(), monitorRequest{ID: "123-aabbccdd", Kind: "full_test", Repo: "owner/project", Payload: json.RawMessage(`{}`)})
	if err != nil || result["status"] != "passed" || result["sha"] != head {
		t.Fatalf("result=%v err=%v", result, err)
	}
	var observation map[string]any
	if err := json.Unmarshal(encode(result["causeObservation"]), &observation); err != nil {
		t.Fatal(err)
	}
	if observation["sha"] != head || observation["baseSha"] != head || observation["complete"] != true || observation["passEvidence"] == nil {
		t.Fatalf("observation=%v", observation)
	}
	raw, err := os.ReadFile(filepath.Join(f.Executor.Config.RuntimeDir, "control", "test-observations", "owner__project.json"))
	var saved []map[string]any
	if err != nil || json.Unmarshal(raw, &saved) != nil || len(saved) != 1 || saved[0]["sha"] != head || saved[0]["baseSha"] != head || saved[0]["complete"] != true {
		t.Fatalf("saved=%s err=%v", raw, err)
	}
}

func TestManualMergeEvidenceMatchesTestedCandidateAndBase(t *testing.T) {
	f, progress, base := delayedMergeFixture(t, false)
	var tested TestResult
	called := false
	result, err := f.Executor.mergeRequestedPullObserved(context.Background(), progress, func(value TestResult) {
		called, tested = true, value
	})
	if err != nil || result["status"] != "merged" || !called {
		t.Fatalf("result=%v observed=%v err=%v", result, called, err)
	}
	head := f.git(f.root, "--git-dir", f.bare, "rev-parse", f.Branch)
	if !tested.Passed || tested.Observation["sha"] != head || tested.Observation["baseSha"] != base || tested.Observation["complete"] != true || tested.Observation["passEvidence"] == nil || testEvidenceReason(tested, head, base) != "" {
		t.Fatalf("candidate=%s base=%s test=%+v", head, base, tested)
	}
}

func TestManualMergeReportsMissingCompletion(t *testing.T) {
	f, progress, _ := delayedMergeFixture(t, false)
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "printf 'ℹ tests 1577\\nℹ pass 1576\\nℹ fail 0\\nℹ skipped 1\\n'"}
	result, err := f.Executor.mergeRequestedPullObserved(context.Background(), progress, nil)
	message, _ := result["message"].(string)
	if err != nil || result["status"] != "refused" || !strings.Contains(message, "完了件数") || f.merged {
		t.Fatalf("result=%v err=%v merged=%v", result, err, f.merged)
	}
}

func TestManualMergeSetupFailureReportsRedactedDiagnostics(t *testing.T) {
	f, progress, _ := delayedMergeFixture(t, false)
	f.Executor.Config.Repositories[0].SetupCommand = []string{"/bin/sh", "-c", "printf 'setup stdout\\n'; printf 'setup error idk_secret123\\n' >&2; exit 3"}
	observed := false
	result, err := f.Executor.mergeRequestedPullObserved(context.Background(), progress, func(TestResult) { observed = true })
	message, _ := result["message"].(string)
	if err != nil || result["status"] != "refused" || observed || f.MergePosts != 0 ||
		!strings.Contains(message, "終了コード 3") || !strings.Contains(message, "setup stdout") ||
		!strings.Contains(message, "setup error") || !strings.Contains(message, "[REDACTED]") || strings.Contains(message, "idk_secret123") {
		t.Fatalf("result=%v err=%v observed=%v mergePosts=%d", result, err, observed, f.MergePosts)
	}
}

func TestFullTestRequestSetupFailureReportsRedactedDiagnostics(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.Config.Repositories[0].SetupCommand = []string{"/bin/sh", "-c", "printf 'setup stdout\\n'; printf 'setup error idk_secret123\\n' >&2; exit 3"}
	result, err := f.Executor.fullTestRequest(context.Background(), monitorRequest{ID: "123-aabbccdd", Kind: "full_test", Repo: "owner/project", Payload: json.RawMessage(`{}`)})
	excerpt, _ := result["excerpt"].(string)
	if err != nil || result["status"] != "unrunnable" ||
		!strings.Contains(excerpt, "終了コード 3") || !strings.Contains(excerpt, "setup stdout") ||
		!strings.Contains(excerpt, "setup error") || !strings.Contains(excerpt, "[REDACTED]") || strings.Contains(excerpt, "idk_secret123") {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

func TestFullTestRequestRejectsHeadChangedByTest(t *testing.T) {
	f := newPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	script := "const {test} = require('node:test'); const {execFileSync} = require('node:child_process'); const fs = require('node:fs'); test('change', () => { fs.writeFileSync('changed', 'new'); execFileSync('git', ['add', 'changed']); execFileSync('git', ['-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid', 'commit', '-m', 'changed']); });\n"
	if err := os.WriteFile(filepath.Join(source, "change.test.js"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "change.test.js")
	f.git(source, "commit", "-m", "fixture")
	f.git(source, "push", f.bare, "main")
	f.Executor.Config.Repositories[0].TestCommand = []string{f.Executor.Config.Node, "--test", "change.test.js"}
	result, err := f.Executor.fullTestRequest(context.Background(), monitorRequest{ID: "123-aabbccdd", Kind: "full_test", Repo: "owner/project", Payload: json.RawMessage(`{}`)})
	var observation map[string]any
	if err != nil || json.Unmarshal(encode(result["causeObservation"]), &observation) != nil || result["status"] != "failed" || observation["passed"] != false || observation["complete"] != false || observation["passEvidence"] != nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

func TestSummaryOnlyCommandDoesNotCreatePassEvidence(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "printf '# tests 1\\n# pass 1\\n# fail 0\\n# cancelled 0\\n'"}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	var report struct {
		Fixed TestResult `json:"fixedTest"`
		Cause struct {
			Target map[string]any `json:"target"`
		} `json:"fixedTestCause"`
	}
	if json.Unmarshal(state.Result, &report) != nil || report.Fixed.Passed || report.Cause.Target["complete"] != false {
		t.Fatalf("report=%s", state.Result)
	}
}

func TestPnpmIncompleteEvidenceStopsPipeline(t *testing.T) {
	f := newPipeline(t, "approve")
	pnpm := testPNPM(t)
	f.Executor.Config.Repositories[0].TestCommand = []string{pnpm, "test"}
	original := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, req)
		if !req.Review {
			if writeErr := os.WriteFile(filepath.Join(req.Policy.Tree, ".gitignore"), []byte("node_modules/\npnpm-lock.yaml\npnpm-workspace.yaml\n"), 0600); writeErr != nil {
				t.Fatal(writeErr)
			}
			if writeErr := os.WriteFile(filepath.Join(req.Policy.Tree, "package.json"), []byte(`{"name":"fixture","version":"1.0.0","scripts":{"test":"node --test feature.test.js"}}`), 0600); writeErr != nil {
				t.Fatal(writeErr)
			}
		}
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	var report struct {
		Fixed TestResult `json:"fixedTest"`
		Cause struct {
			Target map[string]any `json:"target"`
		} `json:"fixedTestCause"`
	}
	if json.Unmarshal(state.Result, &report) != nil || report.Fixed.Passed || report.Cause.Target["complete"] != false {
		t.Fatalf("report=%s", state.Result)
	}
}

// testPNPM returns the pnpm on PATH. The worker image ships it (deploy/versions.json);
// a PC verifying outside the image needs it on PATH, together with a node that pnpm
// supports. A missing pnpm fails the test with that instruction; it is never skipped.
func testPNPM(t *testing.T) string {
	t.Helper()
	pnpm, err := exec.LookPath("pnpm")
	if err != nil {
		t.Fatalf("pnpm が PATH にありません（%v）。deploy/versions.json の pnpm と、それが動く node を PATH に入れてください", err)
	}
	return pnpm
}

func TestFullTestRequestSavesAndReportsAfterScheduledExecution(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.Config.Assignments.Implementers = map[string][]string{"owner/project": {"fixture"}}
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "true")
	request := monitorRequest{ID: "123-aabbccdd", Kind: "full_test", Repo: "owner/project", Payload: json.RawMessage(`{}`)}
	f.NextRequest = &request

	var run func(context.Context) (func(context.Context) error, error)
	ctx := t.Context()
	err := f.Executor.ProcessRequestsWithFullTestFor(ctx, []string{request.Repo}, true, func(repo string, execute func(context.Context) (func(context.Context) error, error)) bool {
		if repo != request.Repo {
			t.Fatalf("reserved repository %q", repo)
		}
		run = execute
		return true
	})
	if err != nil || run == nil || len(f.RequestPosts) != 0 {
		t.Fatalf("request was not deferred: err=%v reports=%v", err, f.RequestPosts)
	}
	send, err := run(ctx)
	if err != nil || send == nil || len(f.RequestPosts) != 0 {
		t.Fatalf("test body reported independently: err=%v reports=%v", err, f.RequestPosts)
	}
	expired, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	if err := send(expired); err == nil {
		t.Fatalf("expired delivery: %v", err)
	}
	progress, err := f.Executor.loadRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(progress.Result, &result); err != nil || progress.Stage != "result" || field[string](result, "status") != "passed" || len(f.RequestPosts) != 0 {
		t.Fatalf("result was not saved before delivery: progress=%+v err=%v reports=%v", progress, err, f.RequestPosts)
	}

	f.Executor.requestsNext = time.Time{}
	err = f.Executor.ProcessRequestsWithFullTestFor(ctx, []string{request.Repo}, true, func(string, func(context.Context) (func(context.Context) error, error)) bool {
		t.Fatal("redelivery scheduled an already completed test")
		return false
	})
	if err != nil || len(f.RequestPosts) != 1 || f.RequestPosts[0] != "/api/v1/requests/"+request.ID+"/result" {
		t.Fatalf("redelivery did not report the saved result: reports=%v err=%v", f.RequestPosts, err)
	}
}

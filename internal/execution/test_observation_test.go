package execution

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/process"
	"issue-desk-worker/internal/testcause"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestObservationVersionDoesNotExecuteTreeOutsideSandbox(t *testing.T) {
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	if err := os.MkdirAll(filepath.Join(root, "workspaces"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(tree, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "executed")
	for _, name := range []string{"python", "go"} {
		tool := filepath.Join(tree, name)
		if name == "python" {
			tool = filepath.Join(tree, ".venv", "bin", "python")
			if err := os.MkdirAll(filepath.Dir(tool), 0700); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module example.com/test\ngo 1.23\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tool, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		// The sandbox launcher fails before invoking its command.
		e := &Executor{Config: Configuration{RuntimeDir: root, Codex: "/bin/false", Git: "/bin/false"}}
		command := []string{tool, "-m", "pytest"}
		fixture := "issue22-py-fail.txt"
		if name == "go" {
			command = []string{tool, "test", "./..."}
			fixture = "issue22-go-fail.txt"
		}
		output, err := os.ReadFile(filepath.Join("..", "testcause", "testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		o, _ := e.testObservationContext(context.Background(), &journal.Record{Tree: tree}, command, process.Result{Code: 1, Out: string(output)}, nil, time.Now(), time.Now(), "fixed-test")
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("tree executable ran: %v", err)
		}
		if o["conditionsComplete"] == true {
			t.Fatal("missing version accepted")
		}
	}
}

func TestFullTestBaselineSurvivesRestartAndDoesNotStoreRawOutput(t *testing.T) {
	cfg := Configuration{}
	cfg.RuntimeDir = t.TempDir()
	first := &Executor{Config: cfg}
	m := testcause.Measurement{SHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Source: "full-test", Command: []string{"node", "--test", "test/sample.test.ts"}, Environment: map[string]any{"node": "v24.21.0", "tz": "UTC"}, Code: 1, Started: time.Unix(100, 0), Finished: time.Unix(101, 0), Output: "not ok 1 - sample\n  ---\n  location: 'test/sample.test.ts:2:1'\n  error: raw-diagnostic-do-not-persist\n  ...\n# tests 1\n# pass 0\n# fail 1\n# cancelled 0\n"}
	report := first.classifyTest("owner/project", testcause.Create(m))
	if report["observationStored"] != true {
		t.Fatal(report)
	}
	file := filepath.Join(cfg.RuntimeDir, "control", "test-observations", "owner__project.json")
	raw, err := os.ReadFile(file)
	if err != nil || strings.Contains(string(raw), "raw-diagnostic-do-not-persist") {
		t.Fatalf("保存形式が不正: %v", err)
	}
	second := &Executor{Config: cfg}
	m.BaseSHA = m.SHA
	m.SHA = strings.Repeat("c", 40)
	m.Source = "fixed-test"
	if report := second.classifyTest("owner/project", testcause.Create(m)); report["kind"] != "baseline" {
		t.Fatal(report)
	}
}

func TestFinishBoundsCauseAndStateRequest(t *testing.T) {
	root := t.TempDir()
	store, err := journal.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Config: Configuration{RuntimeDir: root}, Store: store}
	m := testcause.Measurement{SHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Source: "full-test", Command: []string{"node", "--test", "test/sample.test.ts"}, Environment: map[string]any{"node": "v24", "tz": "UTC"}, Code: 1, Started: time.Unix(100, 0), Finished: time.Unix(101, 0), Output: "not ok 1 - sample\n  ---\n  location: 'test/sample.test.ts:2:1'\n  error: broken\n  ...\n# tests 1\n# pass 0\n# fail 1\n# cancelled 0\n"}
	baseline := testcause.Create(m)
	m.BaseSHA, m.SHA, m.Source = m.SHA, strings.Repeat("c", 40), "fixed-test"
	target := testcause.Create(m)
	baselines := make([]any, 50)
	for i := range baselines {
		baselines[i] = baseline
	}
	dir := filepath.Join(root, "control", "test-observations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "owner__project.json"), encode(baselines), 0600); err != nil {
		t.Fatal(err)
	}
	if got := testcause.Classify(target, baselines); got["kind"] != "baseline" || len(encode(got)) <= 24*1024 {
		t.Fatalf("fixture does not exercise oversized evidence: kind=%v size=%d", got["kind"], len(encode(got)))
	}
	for _, large := range []bool{false, true} {
		r := &journal.Record{ID: 1, Epoch: 1, Repo: "owner/project", Result: encode(map[string]any{"text": strings.Repeat("x", 100)}), Test: encode(TestResult{Observation: target})}
		if large {
			r.Result = encode(map[string]any{"text": strings.Repeat("x", 100000)})
			r.Review = encode(map[string]any{"text": strings.Repeat("y", 100000)})
			r.Test = encode(TestResult{Observation: target, Detail: strings.Repeat("z", 100000)})
		}
		state, err := e.finish(r, "failed", "", "失敗", nil)
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(state.Result, &result); err != nil {
			t.Fatal(err)
		}
		var cause map[string]any
		if err := json.Unmarshal(result["fixedTestCause"], &cause); err != nil {
			t.Fatal(err)
		}
		if len(result["fixedTestCause"]) > 24*1024 || cause["kind"] != "unknown" || cause["target"] != nil {
			t.Fatalf("cause size=%d: %v", len(result["fixedTestCause"]), cause["kind"])
		}
		if len([]rune(cause["acquisition"].(string))) > 500 {
			t.Fatal("acquisition too long")
		}
		body := encode(struct {
			Epoch int64 `json:"epoch"`
			intake.State
		}{r.Epoch, state})
		if len(body) > 128*1024 {
			t.Fatalf("request size=%d", len(body))
		}
	}
}

// Exercise the producer-to-persisted-validator connection for each supported
// command family. Runner output and generated reports are disposable fixtures.
func TestObservationRunnerPassEvidence(t *testing.T) {
	const xml = `<testsuite tests="1" failures="0" errors="0"><testcase name="works"/></testsuite>`
	for _, runner := range []struct {
		name    string
		command []string
		output  string
		files   map[string]string
	}{
		{"Go", []string{"go", "test", "./..."}, "ok  example.com/fixture  0.012s\n", map[string]string{"go.mod": "module example.com/fixture\n", "sample_test.go": "package fixture\n"}},
		{"node", []string{"{node}", "--test", "test/sample.test.js"}, "# tests 1\n# pass 1\n# fail 0\n# cancelled 0\n# skipped 0\n# todo 0\n", map[string]string{"test/sample.test.js": "// test fixture\n"}},
		{"pytest", []string{"{python}", "-m", "pytest", "-q"}, "1 passed in 0.01s\n", nil},
		{"Gradle", []string{"gradle", "--no-daemon", "test"}, "BUILD SUCCESSFUL in 1s\n", map[string]string{"build/test-results/test/TEST-sample.xml": xml}},
		{"Flutter", []string{"flutter", "test"}, "\x1b[2K00:00 +0: loading test/widget_test.dart\r00:01 +1: All tests passed!\n", map[string]string{"test/widget_test.dart": "void main() {}\n"}},
	} {
		states := []string{"success"}
		if runner.name == "Gradle" || runner.name == "Flutter" {
			states = append(states, "missing evidence", "nonzero", "execution error", "interrupted", "output limit", "missing environment", "dirty tree")
		}
		for _, state := range states {
			t.Run(runner.name+"/"+state, func(t *testing.T) {
				root := t.TempDir()
				tree := filepath.Join(root, "tree")
				for _, dir := range []string{tree, filepath.Join(root, "workspaces")} {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				write := func(path, content string, mode os.FileMode) {
					t.Helper()
					if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(content), mode); err != nil {
						t.Fatal(err)
					}
				}
				started := time.Unix(1700000000, 0)
				for name, content := range runner.files {
					if state == "missing evidence" {
						continue
					}
					path := filepath.Join(tree, name)
					write(path, content, 0600)
					at := started.Add(time.Second)
					if err := os.Chtimes(path, at, at); err != nil {
						t.Fatal(err)
					}
				}
				head, base := strings.Repeat("a", 40), strings.Repeat("b", 40)
				git := filepath.Join(root, "git-fixture")
				gitStatus := ""
				if state == "dirty tree" {
					gitStatus = "?? changed"
				}
				// 前に付く -c の数に依らず、サブコマンドで答える。
				write(git, "#!/bin/sh\nfor a; do case \"$a\" in\nrev-parse) echo "+head+"; exit;;\nstatus) echo '"+gitStatus+"'; exit;;\nesac; done\n", 0700)
				node, python, sandbox := filepath.Join(root, "node"), filepath.Join(root, "python"), filepath.Join(root, "sandbox")
				write(node, "#!/bin/sh\necho v24.0.0\n", 0700)
				write(python, "#!/bin/sh\nexit 1\n", 0700)
				write(sandbox, "#!/bin/sh\necho fixture-runtime-version\n", 0700)
				e := &Executor{Config: Configuration{RuntimeDir: root, Git: git, Node: node, Python: python, Codex: sandbox}}
				if state == "missing environment" {
					e.Config.Node = ""
				}
				out := process.Result{Code: 0, Out: runner.output}
				var runErr error
				switch state {
				case "nonzero":
					out.Code = 1
				case "execution error":
					runErr = errors.New("runner could not finish")
				case "interrupted":
					out.Interrupted = true
				case "output limit":
					runErr = process.ErrOutputLimit
				case "missing evidence":
					// Unrelated Go evidence must not rescue a direct run with no
					// Gradle/Flutter evidence through the wrapper fallback.
					write(filepath.Join(tree, "go.mod"), "module example.com/borrowed\n", 0600)
					write(filepath.Join(tree, "borrowed_test.go"), "package borrowed\n", 0600)
					out.Out += "\nok  example.com/borrowed  0.001s\n"
				}
				o, _ := e.testObservationContext(context.Background(), &journal.Record{Tree: tree, Base: base}, runner.command, out, runErr, started, started.Add(2*time.Second), "fixed-test")
				wantPass := state == "success"
				pass, _ := o["passEvidence"].(testcause.Object)
				if (o["complete"] == true) != wantPass || (pass != nil) != wantPass {
					t.Fatalf("unexpected observation: %#v", o)
				}
				store, err := journal.Open(filepath.Join(root, "journal"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				record := &journal.Record{ID: 1, Test: encode(TestResult{Passed: wantPass, Observation: o})}
				if err := store.Save(record); err != nil {
					t.Fatal(err)
				}
				saved, err := store.Load(1)
				if err != nil {
					t.Fatal(err)
				}
				var recovered TestResult
				if err := json.Unmarshal(saved.Test, &recovered); err != nil {
					t.Fatal(err)
				}
				target, _ := e.classifyTest("owner/project", recovered.Observation)["target"].(testcause.Object)
				if (target["complete"] == true) != wantPass {
					t.Fatalf("classified: %#v", target)
				}
				if !wantPass {
					if testEvidenceReason(recovered, head, base) == "" {
						t.Fatal("incomplete evidence accepted")
					}
					return
				}
				proof, _ := target["passEvidence"].(testcause.Object)
				completed, _ := proof["record"].(testcause.Object)
				if files, _ := completed["files"].(float64); files < 1 {
					t.Fatalf("missing completed files: %#v", proof)
				}
				if reason := testEvidenceReason(recovered, head, base); reason != "" {
					t.Fatal(reason)
				}
				if testEvidenceReason(recovered, strings.Repeat("c", 40), base) == "" || testEvidenceReason(recovered, head, strings.Repeat("d", 40)) == "" {
					t.Fatal("identity mismatch accepted")
				}
			})
		}
	}
}

func nodeRecursiveFixture(t *testing.T) (string, *Executor, *journal.Record, []string) {
	t.Helper()
	root := t.TempDir()
	tree := filepath.Join(root, "tree")
	for _, name := range []string{"test/direct.test.ts", "test/a/b/nested.test.ts", "test/helper.ts", "test/other.test.js", "test/a/single.test.mjs", "node_modules/x.test.ts", ".git/x.test.ts", "vendor/x.test.ts"} {
		path := filepath.Join(tree, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "escape.test.ts"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "test", "link")); err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	git := filepath.Join(root, "git")
	node := filepath.Join(root, "node")
	if err := os.WriteFile(git, []byte("#!/bin/sh\nfor a; do case \"$a\" in rev-parse) echo "+head+";; status) exit 0;; esac; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(node, []byte("#!/bin/sh\necho v24.0.0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return tree, &Executor{Config: Configuration{RuntimeDir: root, Git: git, Node: node}}, &journal.Record{Tree: tree, Base: strings.Repeat("b", 40)}, []string{"node", "--test", "test/**/*.test.ts"}
}

func TestNodeRecursiveGlobCompletion(t *testing.T) {
	tree, _, _, cmd := nodeRecursiveFixture(t)
	cases := []struct {
		args []string
		want int
	}{
		{cmd, 2}, {[]string{"node", "--test", "test/**/*.test.ts", "test/direct.test.ts", "../outside/*.test.ts", "test/link/*.test.ts", "node_modules/*.test.ts", ".git/*.test.ts", "vendor/*.test.ts"}, 2},
		{[]string{"node", "--test", "test/direct.test.ts"}, 1}, {[]string{"node", "--test", "test/*.test.ts"}, 1}, {[]string{"node", "--test", "test/**/*.test.{mjs,ts}"}, 3},
	}
	for _, c := range cases {
		if got := completedTestFiles(tree, c.args); got != c.want {
			t.Errorf("%v: got %d want %d", c.args, got, c.want)
		}
	}
}

func TestNodeRecursiveGlobEvidence(t *testing.T) {
	_, e, record, cmd := nodeRecursiveFixture(t)
	output := "# tests 1512\n# suites 0\n# pass 1511\n# fail 0\n# cancelled 0\n# skipped 1\n# todo 0\n# duration_ms 123\n"
	for _, state := range []string{"success", "zero", "failure", "incomplete", "interrupted", "missing", "fewer tests than files"} {
		t.Run(state, func(t *testing.T) {
			command := append([]string(nil), cmd...)
			out := process.Result{Code: 0, Out: output}
			var runErr error
			switch state {
			case "zero":
				command[2] = "none/**/*.test.ts"
			case "failure":
				out.Code = 1
				out.Out = strings.Replace(output, "# fail 0", "# fail 1", 1)
			case "incomplete":
				runErr = process.ErrOutputLimit
			case "interrupted":
				out.Interrupted = true
			case "missing":
				out.Out = ""
			case "fewer tests than files":
				out.Out = "# tests 1\n# pass 1\n# fail 0\n# cancelled 0\n# skipped 0\n# todo 0\n"
			}
			start := time.Unix(1700000000, 0)
			o, _ := e.testObservationContext(context.Background(), record, command, out, runErr, start, start.Add(time.Second), "fixed-test")
			var saved TestResult
			if err := json.Unmarshal(encode(TestResult{Passed: true, Observation: o}), &saved); err != nil {
				t.Fatal(err)
			}
			reason := testEvidenceReason(saved, strings.Repeat("a", 40), record.Base)
			if state == "success" {
				if reason != "" || fullTestFiles(saved) != 2 {
					t.Fatal(reason, o)
				}
				proof := o["passEvidence"].(map[string]any)
				if proof["record"].(map[string]any)["files"] != float64(2) {
					t.Fatal(proof)
				}
			} else if reason == "" || o["complete"] == true || fullTestFiles(saved) != 0 {
				t.Fatalf("%s accepted: %#v", state, o)
			}
		})
	}
}

func TestNodeSelectionFullTestCompletion(t *testing.T) {
	_, e, record, _ := nodeRecursiveFixture(t)
	start := time.Unix(1700000000, 0)
	// A single file can produce several passing tests. The total alone cannot
	// establish coverage when the command deliberately selects a subset.
	out := process.Result{Out: "# tests 9\n# pass 9\n# fail 0\n# cancelled 0\n# skipped 0\n# todo 0\n"}
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"file shard", []string{"--test", "--test-shard=1/2", "test/direct.test.ts", "test/a/b/nested.test.ts"}, 0},
		{"glob name filter", []string{"--test", "--test-name-pattern", "^smoke", "test/*.test.ts"}, 0},
		{"file only", []string{"--test", "--test-only", "test/direct.test.ts"}, 0},
		{"script argument", []string{"other.js", "--test", "--test-only", "test/direct.test.ts"}, 0},
		{"split name filter before test mode", []string{"--test-name-pattern", "^smoke", "--test", "test/direct.test.ts"}, 0},
		{"configured skip and safety flags", []string{"--no-addons", "--disable-proto=throw", "--test", "--test-skip-pattern=^configured exclusion$", "test/*.test.ts"}, 1},
		{"bare", []string{"--test"}, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := append([]string{"node"}, tc.args...)
			o, _ := e.testObservationContext(context.Background(), record, command, out, nil, start, start.Add(time.Second), "full-test")
			result := TestResult{Passed: true, Command: command, Observation: o}
			proof, _ := o["passEvidence"].(testcause.Object)
			if got := fullTestFiles(result); got != tc.want || (proof != nil) != (tc.want > 0) || (o["complete"] == true) != (tc.want > 0) {
				t.Fatalf("files = %d, want %d; observation = %#v", got, tc.want, o)
			}
			if tc.want > 0 {
				result.Observation = nil
				if fullTestFiles(result) != 0 {
					t.Fatal("missing evidence counted as completed files")
				}
			}
		})
	}

	for _, testMode := range [][]string{
		{"--conditions", "development", "--test"},
		{"--title", "verification", "--test"},
		{"--test=true"},
	} {
		t.Run(testMode[0]+" with a shard", func(t *testing.T) {
			node, err := exec.LookPath("node")
			if err != nil {
				t.Skip("Node is unavailable")
			}
			tree, e, record, _ := nodeRecursiveFixture(t)
			for _, name := range []string{"a.test.js", "b.test.js"} {
				if err := os.WriteFile(filepath.Join(tree, name), []byte("require('node:test')('passes', () => {})\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			args := append(testMode, "--test-shard=1/2", "a.test.js", "b.test.js")
			cmd := exec.Command(node, args...)
			cmd.Dir = tree
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("Node failed: %v\n%s", err, output)
			}
			if got := testcause.NodePassedTestCount(string(output)); got != 1 {
				t.Fatalf("Node ran %d tests, want 1\n%s", got, output)
			}

			command := append([]string{node}, args...)
			out := process.Result{Out: string(output)}
			o, _ := e.testObservationContext(context.Background(), record, command, out, nil, start, start.Add(time.Second), "full-test")
			result := TestResult{Passed: true, Command: command, Observation: o}
			proof, _ := o["passEvidence"].(testcause.Object)
			if got := fullTestFiles(result); got != 0 || proof != nil || o["complete"] == true {
				t.Fatalf("files = %d, want 0 with no evidence; observation = %#v", got, o)
			}
		})
	}
}

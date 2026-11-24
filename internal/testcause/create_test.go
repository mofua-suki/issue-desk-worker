package testcause

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func measured() Measurement {
	return Measurement{SHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Source: "fixed-test", Tree: "/tmp/tree", Command: []string{"node", "--test", "test/example.test.ts"}, Environment: Object{"node": "v24.16.0", "tz": "UTC"}, Code: 1, Started: time.Unix(100, 0), Finished: time.Unix(101, 0), Output: "not ok 1 - example\n  ---\n  duration_ms: 12.2\n  location: '/tmp/tree/test/example.test.ts:10:1'\n  error: mismatch\n  ...\n# tests 1\n# pass 0\n# fail 1\n# cancelled 0\n"}
}
func TestMeasuredFailureRoundtrip(t *testing.T) {
	m := measured()
	o := Create(m)
	if o["complete"] != true || len(arr(o["failures"])) != 1 {
		t.Fatalf("不完全な観測: %#v", o)
	}
	raw, _ := json.Marshal(o)
	var recovered any
	_ = json.Unmarshal(raw, &recovered)
	if cleanObservation(recovered)["complete"] != true {
		t.Fatal("再読込で証拠が失われました")
	}
	baseline := Create(m)
	baseline["sha"] = m.BaseSHA
	report := Classify(o, []any{baseline})
	if report["kind"] != "baseline" {
		t.Fatalf("基点判定: %#v", report)
	}
}
func TestMeasurementDurationInvariantAndDiagnosticChange(t *testing.T) {
	m := measured()
	a := Create(m)
	m.Output = strings.Replace(m.Output, "12.2", "123.4", 1)
	b := Create(m)
	if obj(arr(a["failures"])[0])["fingerprint"] != obj(arr(b["failures"])[0])["fingerprint"] {
		t.Fatal("測定時間で指紋が変わりました")
	}
	m.Output = strings.Replace(m.Output, "mismatch", "different", 1)
	c := Create(m)
	if obj(arr(a["failures"])[0])["fingerprint"] == obj(arr(c["failures"])[0])["fingerprint"] {
		t.Fatal("診断差を取り落としました")
	}
}
func TestMeasurementRefusesPartialEvidence(t *testing.T) {
	for _, alter := range []func(*Measurement){func(m *Measurement) { m.Truncated = true }, func(m *Measurement) { m.Interrupted = true }, func(m *Measurement) { m.Output = strings.Replace(m.Output, "# fail 1", "# fail 2", 1) }, func(m *Measurement) { delete(m.Environment, "node") }} {
		m := measured()
		alter(&m)
		if Create(m)["complete"] == true {
			t.Fatal("不完全な出力を完全としました")
		}
	}
}
func TestMeasurementFullPassAndPermission(t *testing.T) {
	m := measured()
	m.Code = 0
	m.Source = "full-test"
	m.Output = ""
	m.CompletedFiles = 1
	o := Create(m)
	if o["complete"] != true || obj(o["passEvidence"]) == nil {
		t.Fatalf("合格の測定証拠: %#v", o)
	}
	m = measured()
	m.Output = "Error: EACCES: permission denied, open '/tmp/outside/data'"
	o = Create(m)
	if o["complete"] != true || o["environmentFailure"] != "permission" {
		t.Fatalf("環境証拠: %#v", o)
	}
	m.Output = "Error: EACCES: permission denied, open '/tmp/tree/data'"
	if Create(m)["complete"] == true {
		t.Fatal("作業領域内の失敗を環境としました")
	}
	m.Output = "[ERR_PNPM_META_FETCH_FAIL] GET https://registry.npmjs.org/typescript: fetch failed\n\n[WARN] Issue while reading \"/home/node/.npmrc\". EACCES: permission denied, open '/home/node/.npmrc'\n"
	if o := Create(m); o["environmentFailure"] != nil || obj(o["permissionEvidence"]) != nil {
		t.Fatalf("pnpmの警告を権限の環境問題としました: %#v", o)
	}
}

func TestFixedPassMeasurementAndIncompleteRuns(t *testing.T) {
	m := measured()
	m.Code, m.Output, m.CompletedFiles = 0, "", 1
	o := Create(m)
	if o["complete"] != true || o["sha"] != m.SHA || o["source"] != "fixed-test" {
		t.Fatalf("fixed pass: %#v", o)
	}
	goRun := m
	goRun.Command = []string{"go", "test", "./..."}
	goRun.Environment = Object{"go": "go1.27", "tz": "UTC"}
	goRun.Output = "ok  example.com/project  0.012s\n"
	if got := Create(goRun); got["complete"] != true || got["sha"] != m.SHA {
		t.Fatalf("Go fixed pass: %#v", got)
	}
	goRun.Environment = Object{"node": "v24", "tz": "UTC"}
	if got := Create(goRun); got["complete"] == true {
		t.Fatalf("Go実行でGoの版が欠けた証拠を採用: %#v", got)
	}
	for name, alter := range map[string]func(*Measurement){
		"missing sha":        func(m *Measurement) { m.SHA = "" },
		"invalid sha":        func(m *Measurement) { m.SHA = "invalid" },
		"interrupted":        func(m *Measurement) { m.Interrupted = true },
		"truncated":          func(m *Measurement) { m.Truncated = true },
		"missing conditions": func(m *Measurement) { delete(m.Environment, "node") },
		"no completed files": func(m *Measurement) { m.CompletedFiles = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			m := measured()
			m.Code, m.Output, m.CompletedFiles = 0, "", 1
			alter(&m)
			if got := Create(m); got["complete"] == true {
				t.Fatalf("accepted incomplete pass: %#v", got)
			}
		})
	}
}

func TestPassRejectsContradictorySummaries(t *testing.T) {
	for _, source := range []string{"fixed-test", "full-test"} {
		for _, output := range []string{
			"# tests 1\n# pass 0\n# fail 1\n# cancelled 0\n",
			"ℹ tests 1\nℹ pass 0\nℹ fail 0\nℹ cancelled 1\n",
			"# fail 1\n# fail 0\n",
			"# fail 999999999999999999999999999999999999\n",
		} {
			m := measured()
			m.Source, m.Code, m.Output, m.CompletedFiles = source, 0, output, 1
			o := Create(m)
			raw, err := json.Marshal(o)
			if err != nil {
				t.Fatal(err)
			}
			var recovered Object
			if err := json.Unmarshal(raw, &recovered); err != nil {
				t.Fatal(err)
			}
			target := obj(Report(Object{"target": recovered}, 24576)["target"])
			if o["complete"] == true || obj(o["passEvidence"]) != nil || target["complete"] == true {
				t.Fatalf("accepted contradictory %s output %q: %#v", source, output, target)
			}
		}
	}
	m := measured()
	m.Code, m.CompletedFiles, m.Output = 0, 1, "# tests 1\n# pass 1\n# fail 0\n# cancelled 0\n"
	if o := Create(m); o["complete"] != true || o["passEvidence"] == nil {
		t.Fatalf("rejected passing summary: %#v", o)
	}
}

func TestForeignReports(t *testing.T) {
	for _, tc := range []struct {
		file, runtime string
		complete      bool
	}{
		{"issue22-go-fail.txt", "go", true}, {"issue22-go-pass.txt", "go", false},
		{"issue22-go-build.txt", "go", false}, {"issue22-go-truncated.txt", "go", false},
		{"issue22-py-fail.txt", "python", true}, {"issue22-py-pass.txt", "python", false},
		{"issue22-py-build.txt", "python", false}, {"issue22-py-truncated.txt", "python", false},
		{"issue22-py-multiframe-slow.txt", "python", true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			m := measured()
			m.Output = string(raw)
			m.Environment = Object{tc.runtime: "version", "tz": "UTC"}
			o := Create(m)
			if (o["complete"] == true) != tc.complete {
				t.Fatalf("complete: %#v", o)
			}
			if !tc.complete {
				return
			}
			if len(arr(o["failures"])) != 1 {
				t.Fatalf("failures: %#v", o)
			}
			baseline := Create(m)
			baseline["sha"] = m.BaseSHA
			if report := Classify(o, []any{baseline}); report["kind"] != "baseline" {
				t.Fatalf("classification: %#v", report)
			}
			delete(m.Environment, tc.runtime)
			if Create(m)["complete"] == true {
				t.Fatal("missing runtime")
			}
		})
	}
}
func TestForeignReportsRejectAmbiguityAndIncomplete(t *testing.T) {
	for _, file := range []string{"issue22-go-fail.txt", "issue22-py-fail.txt"} {
		raw, err := os.ReadFile(filepath.Join("testdata", file))
		if err != nil {
			t.Fatal(err)
		}
		m := measured()
		m.Output = string(raw)
		m.Environment = Object{"node": "v1", "go": "v1", "python": "v1", "tz": "UTC"}
		a := Create(m)
		m.Truncated = true
		if Create(m)["complete"] == true {
			t.Fatal("truncated")
		}
		m.Truncated = false
		m.Output += "\n# tests 1\n# pass 0\n# fail 1\n# cancelled 0\n"
		if Create(m)["complete"] == true {
			t.Fatal("ambiguous")
		}
		m.Output = strings.ReplaceAll(string(raw), "0.00s", "9.99s")
		m.Output = strings.ReplaceAll(m.Output, "0.01s", "9.99s")
		m.Output = strings.ReplaceAll(m.Output, "0.002s", "9.99s")
		b := Create(m)
		if obj(arr(a["failures"])[0])["fingerprint"] != obj(arr(b["failures"])[0])["fingerprint"] {
			t.Fatal("duration changed fingerprint")
		}
	}
}

func TestForeignReportsCountAndPackageBoundaries(t *testing.T) {
	for _, tc := range []struct{ file, key, old, replacement string }{
		{"issue22-go-fail.txt", "go", "FAIL\texample.com/sample\t0.002s", "FAIL\texample.com/sample\t[build failed]"},
		{"issue22-py-fail.txt", "python", "1 failed, 1 passed", "2 failed, 1 passed"},
	} {
		raw, err := os.ReadFile(filepath.Join("testdata", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		m := measured()
		m.Output = strings.Replace(string(raw), tc.old, tc.replacement, 1)
		m.Environment = Object{tc.key: "v1", "tz": "UTC"}
		if Create(m)["complete"] == true {
			t.Fatalf("accepted mismatched output: %s", tc.file)
		}
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "issue22-go-fail.txt"))
	if err != nil {
		t.Fatal(err)
	}
	m := measured()
	m.Environment = Object{"go": "v1", "tz": "UTC"}
	m.Output = string(raw) + strings.ReplaceAll(string(raw), "example.com/sample", "example.com/other")
	if o := Create(m); o["complete"] != true || len(arr(o["failures"])) != 2 {
		t.Fatalf("package names: %#v", o)
	}
}

func TestPytestFailureBodyControlsClassification(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "issue22-py-fail.txt"))
	if err != nil {
		t.Fatal(err)
	}
	m := measured()
	m.Environment = Object{"python": "3.12", "tz": "UTC"}
	m.Output = strings.Replace(string(raw), " - assert 1 == 2", "", 1)
	baseline := Create(m)
	baseline["sha"] = m.BaseSHA
	m.Output = strings.Replace(m.Output, "E       assert 1 == 2", "E       assert 1 == 3", 1)
	target := Create(m)
	if target["complete"] != true || obj(arr(target["failures"])[0])["fingerprint"] == obj(arr(baseline["failures"])[0])["fingerprint"] {
		t.Fatal("failure body did not change fingerprint")
	}
	if Classify(target, []any{baseline})["kind"] == "baseline" {
		t.Fatal("different failure classified as baseline")
	}
	start := strings.Index(m.Output, "=================================== FAILURES")
	end := strings.Index(m.Output, "=========================== short test summary")
	m.Output = m.Output[:start] + m.Output[end:]
	target = Create(m)
	if target["complete"] == true || Classify(target, []any{baseline})["kind"] != "unknown" {
		t.Fatal("missing body used as evidence")
	}
}

// Each form must be readable on its own: a traceback separator is not a failure
// header, and pytest appends the clock form to totals of 60 seconds or more.
func TestPytestMultiFrameAndLongRunEachStayComplete(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "issue22-py-multiframe-slow.txt"))
	if err != nil {
		t.Fatal(err)
	}
	both := string(raw)
	for name, output := range map[string]string{
		"both":        both,
		"multi-frame": strings.Replace(both, "in 75.23s (0:01:15)", "in 0.01s", 1),
		"long-run":    regexp.MustCompile(`(?m)^_ _ .*\n`).ReplaceAllString(both, ""),
		// The terminal width decides whether the separator ends in a space.
		"separator-without-trailing-space": regexp.MustCompile(`(?m)^(_ _ .*_) $`).ReplaceAllString(both, "$1"),
		"day-long":                         strings.Replace(both, "(0:01:15)", "(1 day, 0:00:01)", 1),
	} {
		t.Run(name, func(t *testing.T) {
			m := measured()
			m.Output = output
			m.Environment = Object{"python": "version", "tz": "UTC"}
			o := Create(m)
			if o["complete"] != true || len(arr(o["failures"])) != 1 {
				t.Fatalf("observation: %#v", o)
			}
		})
	}
}

// Wrapper commands (scripts/verify.sh, node --test without files, pytest) name no test
// files: a pass counts only with real test files in the tree and a clean runner summary.
func TestWrapperPassCountsProvenPackages(t *testing.T) {
	verify := "ok  \tissue-desk-intake/server/internal/a\t0.512s\nok  \tissue-desk-intake/server/internal/b\t(cached)\n?   \tissue-desk-intake/server/cmd\t[no test files]\n# tests 12\n# suites 0\n# pass 12\n# fail 0\n# cancelled 0\n# skipped 0\n# todo 0\n"
	m := measured()
	m.Code, m.Output, m.Command, m.Environment, m.CompletedFiles, m.ProvenPackages = 0, verify, []string{"sh", "scripts/verify.sh"}, Object{"node": "v24.16.0", "tz": "UTC"}, 0, 2
	o := Create(m)
	if o["complete"] != true || o["passed"] != true {
		t.Fatalf("wrapper pass stayed incomplete: %#v", o)
	}
	if record := obj(obj(o["passEvidence"])["record"]); record["files"] != float64(2) {
		t.Fatalf("completed count %v, want 2", record["files"])
	}
	if target := obj(Classify(o, nil)["target"]); target["complete"] != true {
		t.Fatalf("revalidated target incomplete: %#v", target)
	}
	for name, output := range map[string]string{
		"no proven packages": verify,
		"no summary":         "all good\n",
		"no test files only": "?   \tx/cmd\t[no test files]\n",
		"zero passes":        "# tests 0\n# pass 0\n# fail 0\n",
		"failed count":       "# tests 3\n# pass 2\n# fail 1\n" + verify,
		"cancelled count":    verify + "# cancelled 1\n",
		"go FAIL line":       verify + "FAIL\tissue-desk-intake/server/internal/c\t0.1s\n",
		"go --- FAIL":        "--- FAIL: TestX (0.00s)\n" + verify,
		"pytest failure":     verify + "=========== 1 failed, 3 passed in 2.00s ===========\n",
		"pytest error":       verify + "=========== 3 passed, 1 error in 2.00s ===========\n",
	} {
		t.Run(name, func(t *testing.T) {
			m := measured()
			m.Code, m.Output, m.Command, m.CompletedFiles, m.ProvenPackages = 0, output, []string{"sh", "scripts/verify.sh"}, 0, 2
			if name == "no proven packages" {
				m.ProvenPackages = 0
			}
			if got := Create(m); got["complete"] == true {
				t.Fatalf("accepted pass without proven packages and a clean summary: %#v", got)
			}
		})
	}
}

func TestGoZeroRunsDoNotProducePassEvidence(t *testing.T) {
	const a = "ok  issue-desk-worker/internal/testcause  0.012s"
	const b = "ok  issue-desk-worker/internal/execution  (cached)"
	for _, tc := range []struct {
		name, output string
		want         int
	}{
		{"zero", a + " [no tests to run]\n", 0},
		{"zero with coverage", a + " coverage: 42.0% of statements [no tests to run]\n", 0},
		{"normal and cached", a + "\n" + b + "\n", 2},
		{"mixed", a + " [no tests to run]\n" + b + "\n", 1},
		{"same package later ran", a + " [no tests to run]\n" + a + "\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proven := ProvenGoPackages("../..", tc.output)
			if proven != tc.want || reportedPasses(tc.output) != tc.want {
				t.Fatalf("proven=%d reported=%d, want %d", proven, reportedPasses(tc.output), tc.want)
			}
			m := measured()
			m.Code, m.Output, m.Command, m.CompletedFiles, m.ProvenPackages = 0, tc.output, []string{"sh", "scripts/verify.sh"}, 0, proven
			m.Environment = Object{"go": "go1.27", "tz": "UTC"}
			target := obj(Classify(Create(m), nil)["target"])
			proof := obj(target["passEvidence"])
			if tc.want == 0 {
				if target["complete"] == true || proof != nil {
					t.Fatalf("zero runs produced evidence: %#v", target)
				}
			} else if target["complete"] != true || obj(proof["record"])["files"] != float64(tc.want) {
				t.Fatalf("incorrect pass evidence: %#v", target)
			}
		})
	}
}

func TestProvenGoPackagesNeedsTreePackagesWithTests(t *testing.T) {
	tree := t.TempDir()
	write := func(path, body string) {
		t.Helper()
		full := filepath.Join(tree, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("server/go.mod", "module example.test/server\n\ngo 1.24\n")
	write("server/internal/a/a_test.go", "package a\n")
	write("server/internal/b/b.go", "package b\n")
	write("node_modules/x/go.mod", "module example.test/vendored\n")
	write("node_modules/x/y/y_test.go", "package y\n")
	output := "ok  \texample.test/server/internal/a\t0.1s\nok  \texample.test/server/internal/a\t(cached)\nok  \texample.test/server/internal/b\t0.1s\nok  \texample.test/server/internal/missing\t0.1s\nok  \texample.test/vendored/y\t0.1s\nok  \tother.test/a\t0.1s\n# pass 3\n"
	if got := ProvenGoPackages(tree, output); got != 1 {
		t.Fatalf("proven packages %d, want 1", got)
	}
	if got := ProvenGoPackages(tree, "# tests 1\n# pass 1\n# fail 0\n"); got != 0 {
		t.Fatalf("printed node summary proved %d packages", got)
	}
}

func TestDirectPytestPassEvidence(t *testing.T) {
	for _, output := range []string{"5601 passed, 2 skipped in 265.44s (0:04:25)", "==== 3 passed in 1.20s ===="} {
		m := measured()
		m.Command = []string{".venv/bin/python", "-m", "pytest", "-q"}
		m.Environment = Object{"python": "Python 3.12", "tz": "UTC"}
		m.Code = 0
		m.Output = output
		o := Create(m)
		raw, err := json.Marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		var saved any
		if err := json.Unmarshal(raw, &saved); err != nil {
			t.Fatal(err)
		}
		target := obj(Classify(saved, nil)["target"])
		if target["complete"] != true || obj(target["passEvidence"]) == nil || obj(obj(target["passEvidence"])["record"])["files"] != float64(1) {
			t.Fatalf("pass evidence: %#v", target)
		}
	}
}

func TestDirectPytestIncompleteEvidence(t *testing.T) {
	cases := map[string]func(*Measurement){
		"missing summary":           func(m *Measurement) { m.Output = "tests finished" },
		"truncated":                 func(m *Measurement) { m.Truncated = true },
		"interrupted":               func(m *Measurement) { m.Interrupted = true },
		"nonzero":                   func(m *Measurement) { m.Code = 1 },
		"failed":                    func(m *Measurement) { m.Output = "1 failed, 3 passed in 1.00s" },
		"error":                     func(m *Measurement) { m.Output = "3 passed, 1 error in 1.00s" },
		"interruption report":       func(m *Measurement) { m.Output += "\n!!!! Interrupted: signal !!!!" },
		"failure alongside summary": func(m *Measurement) { m.Output += "\nFAILED test_a.py::test_a" },
		"invalid sha":               func(m *Measurement) { m.SHA = "bad" },

		"missing python": func(m *Measurement) { delete(m.Environment, "python") },
		"shell":          func(m *Measurement) { m.Command = []string{"sh", "-c", "echo 3 passed in 1.00s"} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := measured()
			m.Command = []string{".venv/bin/python", "-m", "pytest", "-q"}
			m.Environment = Object{"python": "3.12", "node": "v24", "tz": "UTC"}
			m.Code = 0
			m.Output = "3 passed in 1.00s"
			change(&m)
			o := Create(m)
			if o["complete"] == true || obj(o["passEvidence"]) != nil {
				t.Fatalf("unexpected evidence: %#v", o)
			}
		})
	}
}

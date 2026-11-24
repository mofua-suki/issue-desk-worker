package testcause

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nodeTree(t *testing.T, script string) string {
	t.Helper()
	tree := t.TempDir()
	files := map[string]string{
		"test/a.test.ts":                  "",
		"test/helpers/fixture.mjs":        "",
		"src/b.test.mjs":                  "",
		"src/c_test.js":                   "",
		"src/test-d.cjs":                  "",
		"src/plain.ts":                    "",
		"test/readme.md":                  "",
		"node_modules/pkg/test/x.test.js": "",
		".tmp/old/y.test.js":              "",
		".github/test/z.mjs":              "",
	}
	if script != "" {
		files["package.json"] = `{"scripts":{"test":` + script + `}}`
	}
	for name, body := range files {
		path := filepath.Join(tree, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tree
}

func TestProvenNodeTestFilesFollowsDefaultPatterns(t *testing.T) {
	tree := nodeTree(t, `"node --test"`)
	for name, command := range map[string][]string{
		"direct":       {"/usr/local/bin/node", "--test"},
		"direct flags": {"node", "--test", "--test-concurrency=1", "--test-reporter=spec"},
		// sample-scraper's configured command (the intake's fleet.json).
		"configured skip": {"node", "--test", "--test-skip-pattern=^(fixed HTML extracts photo ownership|headed browser enables sandboxing)"},
		"pnpm test":       {"pnpm", "test"},
		"pnpm run":        {"pnpm", "run", "test"},
		"npm test":        {"npm", "test"},
	} {
		// test/a.test.ts, test/helpers/fixture.mjs, src/b.test.mjs, src/c_test.js, src/test-d.cjs
		if got := ProvenNodeTestFiles(tree, command, nodeBareSummary); got != 5 {
			t.Fatalf("%s: got %d, want 5", name, got)
		}
	}
}

func TestProvenNodeTestFilesRejectsOtherCommands(t *testing.T) {
	tree := nodeTree(t, `"node --test"`)
	for name, command := range map[string][]string{
		"file argument": {"node", "--test", "test/a.test.ts"},
		"no --test":     {"node", "src/plain.ts"},
		"go":            {"go", "test", "./..."},
		"pnpm other":    {"pnpm", "lint"},
		"empty":         {},
		"shard":         {"node", "--test", "--test-shard=1/50"},
		"only":          {"node", "--test", "--test-only"},
		"name pattern":  {"node", "--test", "--test-name-pattern=^smoke"},
		"import":        {"node", "--import=./x.mjs", "--test"},
		"reporter":      {"node", "--test", "--test-reporter=./custom.mjs"},
		"node-gyp":      {"node-gyp", "--test"},
		"empty skip":    {"node", "--test", "--test-skip-pattern="},
	} {
		if got := ProvenNodeTestFiles(tree, command, nodeBareSummary); got != 0 {
			t.Fatalf("%s: got %d, want 0", name, got)
		}
	}
	for _, script := range []string{`"node --test && echo ok"`, `"vitest run"`, `"node --test test/a.test.ts"`, `""`, `"node --test --test-shard=1/2"`, `"node --test --test-skip-pattern=."`} {
		if got := ProvenNodeTestFiles(nodeTree(t, script), []string{"pnpm", "test"}, nodeBareSummary); got != 0 {
			t.Fatalf("script %s: got %d, want 0", script, got)
		}
	}
	if got := ProvenNodeTestFiles(nodeTree(t, ""), []string{"pnpm", "test"}, nodeBareSummary); got != 0 {
		t.Fatalf("no package.json: got %d, want 0", got)
	}
}

// The tail of sample-scraper PR #875's fixed test (mofua-suki/sample-scraper),
// run as a bare `node --test`: every test passed and one was skipped.
const nodeBareSummary = "✔ the documents describe the X /static/ redirect as a lost image (3.537253ms)\nℹ tests 1577\nℹ suites 0\nℹ pass 1576\nℹ fail 0\nℹ cancelled 0\nℹ skipped 1\nℹ todo 0\nℹ duration_ms 107320.886764\n"

func TestBareNodeTestPassBecomesCompleteEvidence(t *testing.T) {
	tree := nodeTree(t, `"node --test"`)
	command := []string{"pnpm", "test"}
	m := measured()
	m.Code, m.Output, m.Command, m.Environment, m.CompletedFiles, m.ProvenPackages = 0, nodeBareSummary, command, Object{"node": "v24.22.3", "tz": "UTC"}, 0, ProvenNodeTestFiles(tree, command, nodeBareSummary)
	o := Create(m)
	if o["complete"] != true || o["passed"] != true {
		t.Fatalf("bare node --test pass stayed incomplete: %#v", o)
	}
	if record := obj(obj(o["passEvidence"])["record"]); record["files"] != float64(5) {
		t.Fatalf("completed count %v, want 5", record["files"])
	}
	if target := obj(Classify(o, nil)["target"]); target["complete"] != true {
		t.Fatalf("revalidated target incomplete: %#v", target)
	}
	for name, output := range map[string]string{
		"failure":                strings.Replace(nodeBareSummary, "ℹ fail 0", "ℹ fail 1", 1),
		"cancelled":              strings.Replace(nodeBareSummary, "ℹ cancelled 0", "ℹ cancelled 1", 1),
		"no summary":             "✔ something (1ms)\n",
		"pass only":              "ℹ pass 3\n",
		"mismatch":               strings.Replace(nodeBareSummary, "ℹ tests 1577", "ℹ tests 1580", 1),
		"all skipped":            strings.NewReplacer("ℹ pass 1576", "ℹ pass 0", "ℹ skipped 1", "ℹ skipped 1577").Replace(nodeBareSummary),
		"duplicate":              nodeBareSummary + "ℹ pass 4\n",
		"fewer tests than files": "ℹ tests 2\nℹ pass 2\nℹ fail 0\nℹ cancelled 0\nℹ skipped 0\nℹ todo 0\n",
		"go failure":             nodeBareSummary + "--- FAIL: TestX (0.00s)\n",
	} {
		t.Run(name, func(t *testing.T) {
			m := measured()
			m.Code, m.Output, m.Command, m.Environment, m.CompletedFiles, m.ProvenPackages = 0, output, command, Object{"node": "v24.22.3", "tz": "UTC"}, 0, ProvenNodeTestFiles(tree, command, output)
			if o := Create(m); o["complete"] == true && o["passed"] == true {
				t.Fatalf("accepted %s: %#v", name, o)
			}
		})
	}
}

func TestBareNodeTestRunStateStillRejects(t *testing.T) {
	tree := nodeTree(t, `"node --test"`)
	command := []string{"node", "--test"}
	for name, change := range map[string]func(*Measurement){
		"exit code":   func(m *Measurement) { m.Code = 1 },
		"truncated":   func(m *Measurement) { m.Truncated = true },
		"interrupted": func(m *Measurement) { m.Interrupted = true },
	} {
		t.Run(name, func(t *testing.T) {
			m := measured()
			m.Code, m.Output, m.Command, m.Environment, m.CompletedFiles, m.ProvenPackages = 0, nodeBareSummary, command, Object{"node": "v24.22.3", "tz": "UTC"}, 0, ProvenNodeTestFiles(tree, command, nodeBareSummary)
			change(&m)
			if o := Create(m); o["passed"] == true || o["complete"] == true {
				t.Fatalf("accepted %s: %#v", name, o)
			}
		})
	}
}

func TestProvenNodeTestFilesNeedsTestFiles(t *testing.T) {
	tree := t.TempDir()
	if got := ProvenNodeTestFiles(tree, []string{"node", "--test"}, nodeBareSummary); got != 0 {
		t.Fatalf("empty tree: got %d", got)
	}
}

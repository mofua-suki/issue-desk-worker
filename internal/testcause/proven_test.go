package testcause

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProvenGradleResults(t *testing.T) {
	const good = `<testsuite tests="2" failures="0" errors="0"><testcase name="works"/></testsuite>`
	started := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name string
		xmls []string
		age  time.Duration
		kind string
		want int
	}{
		{name: "current suites", xmls: []string{good, good}, age: time.Second, want: 2},
		{name: "current and old broken suite", xmls: []string{good, `<testsuite`}, age: time.Second, kind: "old second", want: 1},
		{name: "missing"},
		{name: "old only", xmls: []string{good}, age: -time.Second},
		{name: "not after start", xmls: []string{good}},
		{name: "zero tests", xmls: []string{`<testsuite tests="0" failures="0" errors="0"/>`}, age: time.Second},
		{name: "failures", xmls: []string{`<testsuite tests="2" failures="1" errors="0"/>`}, age: time.Second},
		{name: "errors", xmls: []string{`<testsuite tests="2" failures="0" errors="1"/>`}, age: time.Second},
		{name: "missing attribute", xmls: []string{`<testsuite tests="2" failures="0"/>`}, age: time.Second},
		{name: "negative attribute", xmls: []string{`<testsuite tests="-2" failures="0" errors="0"/>`}, age: time.Second},
		{name: "invalid attribute", xmls: []string{`<testsuite tests="two" failures="0" errors="0"/>`}, age: time.Second},
		{name: "overflow attribute", xmls: []string{`<testsuite tests="99999999999999999999" failures="0" errors="0"/>`}, age: time.Second},
		{name: "duplicate attribute", xmls: []string{`<testsuite tests="2" tests="2" failures="0" errors="0"/>`}, age: time.Second},
		{name: "broken xml", xmls: []string{`<testsuite tests="2" failures="0" errors="0">`}, age: time.Second},
		{name: "trailing junk", xmls: []string{good + "broken"}, age: time.Second},
		{name: "multiple roots", xmls: []string{good + good}, age: time.Second},
		{name: "mixed failure", xmls: []string{good, `<testsuite tests="2" failures="1" errors="0"/>`}, age: time.Second},
		{name: "mixed incomplete", xmls: []string{good, `<testsuite tests="2"/>`}, age: time.Second},
		{name: "nested incomplete suite", xmls: []string{`<testsuite tests="2" failures="0" errors="0"><testsuite tests="2"/></testsuite>`}, age: time.Second},
		{name: "failure element", xmls: []string{`<testsuite tests="2" failures="0" errors="0"><testcase><failure/></testcase></testsuite>`}, age: time.Second},
		{name: "external reference", xmls: []string{`<!DOCTYPE testsuite SYSTEM "outside.xml">` + good}, age: time.Second},
		{name: "nonregular result", xmls: []string{good}, age: time.Second, kind: "directory"},
		{name: "linked result", xmls: []string{good}, age: time.Second, kind: "file link"},
		{name: "linked results directory", xmls: []string{good}, age: time.Second, kind: "directory link"},
		{name: "dangling result", xmls: []string{good}, age: time.Second, kind: "dangling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := t.TempDir()
			dir := filepath.Join(tree, "build", "test-results", "test")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for i, content := range tc.xmls {
				path := filepath.Join(dir, "TEST-"+strings.Repeat("x", i+1)+".xml")
				if tc.kind == "directory" {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
				at := started.Add(tc.age)
				if tc.kind == "old second" && i == 1 {
					at = started.Add(-time.Second)
				}
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			}
			if tc.kind == "file link" || tc.kind == "dangling" {
				path := filepath.Join(dir, "TEST-x.xml")
				outside := filepath.Join(t.TempDir(), "result.xml")
				if err := os.Rename(path, outside); err != nil {
					t.Fatal(err)
				}
				if tc.kind == "dangling" {
					outside += ".missing"
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			if tc.kind == "directory link" {
				outside := filepath.Join(t.TempDir(), "results")
				if err := os.Rename(dir, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, dir); err != nil {
					t.Fatal(err)
				}
			}
			if got := ProvenGradleResults(tree, started); got != tc.want {
				t.Fatalf("completed=%d want=%d", got, tc.want)
			}
		})
	}
}

func TestProvenFlutterResults(t *testing.T) {
	for _, tc := range []struct {
		name, output, kind string
		want               int
	}{
		{"plain terminal", "All tests passed!\n", "files", 2},
		{"compact ANSI progress", "\x1b[?25l\x1b[2K00:00 +0: loading test/widget_test.dart\r\x1b[32m00:01 +2: All tests passed!\x1b[0m\x1b[?25h  \r\n", "files", 2},
		{"skipped progress", "00:01 +1 ~1: All tests passed!\n", "files", 2},
		{"test stdout contains failure words", "value -1 and Test failed; Failed to load; Unhandled exception; [E]\n00:01 +2: All tests passed!\n", "files", 2},
		{"missing files", "All tests passed!", "none", 0},
		{"missing terminal", "00:01 +1: sample", "files", 0},
		{"partial terminal", "All tests passed", "files", 0},
		{"embedded terminal", "test prints All tests passed!", "files", 0},
		{"terminal suffix", "All tests passed! maybe", "files", 0},
		{"output after terminal", "All tests passed!\n00:01 +1: still running", "files", 0},
		{"mixed failure", "00:01 +1 -1: Some tests failed.\rAll tests passed!", "files", 0},
		{"failure after success", "All tests passed!\nSome tests failed.", "files", 0},
		{"failed test progress", "00:01 +1 -1: failed test [E]\rAll tests passed!", "files", 0},
		{"load error", "00:01 +0 -1: loading test/widget_test.dart [E]\nFailed to load test/widget_test.dart\nAll tests passed!", "files", 0},
		{"failure with skipped tests", "00:01 +1 -1 ~1: failed test [E]\nAll tests passed!", "files", 0},
		{"directory named test", "All tests passed!", "directory", 0},
		{"file link", "All tests passed!", "file link", 0},
		{"nested directory link", "All tests passed!", "nested link", 0},
		{"test directory link", "All tests passed!", "root link", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := t.TempDir()
			dir := filepath.Join(tree, "test")
			if err := os.MkdirAll(filepath.Join(dir, "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			switch tc.kind {
			case "files":
				for _, name := range []string{"widget_test.dart", "nested/other_test.dart", "helper.dart"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("void main() {}"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "directory":
				if err := os.Mkdir(filepath.Join(dir, "widget_test.dart"), 0700); err != nil {
					t.Fatal(err)
				}
			case "file link", "nested link", "root link":
				outside := t.TempDir()
				file := filepath.Join(outside, "widget_test.dart")
				if err := os.WriteFile(file, []byte("void main() {}"), 0600); err != nil {
					t.Fatal(err)
				}
				from, to := file, filepath.Join(dir, "widget_test.dart")
				if tc.kind == "nested link" {
					from, to = outside, filepath.Join(dir, "linked")
				}
				if tc.kind == "root link" {
					if err := os.RemoveAll(dir); err != nil {
						t.Fatal(err)
					}
					from, to = outside, dir
				}
				if err := os.Symlink(from, to); err != nil {
					t.Fatal(err)
				}
			}
			if got := ProvenFlutterResults(tree, tc.output); got != tc.want {
				t.Fatalf("completed=%d want=%d", got, tc.want)
			}
		})
	}
}

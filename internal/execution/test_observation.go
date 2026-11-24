package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"issue-desk-worker/internal/atomicfile"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/process"
	"issue-desk-worker/internal/testcause"
)

func (e *Executor) testObservationContext(ctx context.Context, r *journal.Record, command []string, out process.Result, runErr error, started, finished time.Time, source string) (testcause.Object, testcause.Object) {
	resolved := e.resolveTestCommand(command)
	goTest := len(resolved) > 1 && filepath.Base(resolved[0]) == "go" && resolved[1] == "test"
	environment := testcause.Object{"tz": "UTC", "platform": runtime.GOOS, "arch": runtime.GOARCH, "go": runtime.Version()}
	if e.Config.Node != "" {
		version, err := process.Run(ctx, e.Config.Node, []string{"--version"}, process.Options{Timeout: 10 * time.Second})
		if err == nil && version.Code == 0 {
			environment["node"] = strings.TrimSpace(version.Out)
		}
	}
	if len(resolved) > 0 {
		executable := filepath.Base(resolved[0])
		if executable == "python" || strings.HasPrefix(executable, "python3") {
			version, err := e.sandbox(ctx, r, []string{resolved[0], "--version"}, 10)
			if err == nil && version.Code == 0 {
				environment["python"] = strings.TrimSpace(version.Out + version.Err)
			}
		}
		if goTest {
			delete(environment, "go")
			version, err := e.sandbox(ctx, r, []string{resolved[0], "version"}, 10)
			if err == nil && version.Code == 0 {
				environment["go"] = strings.TrimSpace(version.Out)
			}
		}
	}
	head := ""
	rev, err := e.gitConfig().Local(ctx, r.Tree, 10*time.Second, "rev-parse", "HEAD")
	if err == nil && rev.Code == 0 {
		head = strings.TrimSpace(rev.Out)
	}
	// A pre-commit dirty tree cannot be attributed to its parent commit.
	status, err := e.gitConfig().Local(ctx, r.Tree, 10*time.Second, "status", "--porcelain")
	if err != nil || status.Code != 0 || strings.TrimSpace(status.Out) != "" {
		head = ""
	}
	code := out.Code
	if runErr != nil && code == 0 {
		code = -1
	}
	var environmentError testEnvironmentError
	interrupted := out.Interrupted || runErr != nil && !errors.As(runErr, &environmentError)
	output := e.redact(out.Out + "\n" + out.Err)
	completed, proven := 0, 0
	// Direct Gradle/Flutter runs require their own execution evidence, even when
	// arguments or unrelated output would otherwise look like Node/Go tests.
	runner := ""
	if len(resolved) > 1 {
		switch filepath.Base(resolved[0]) {
		case "gradle":
			for _, arg := range resolved[1:] {
				if arg == "test" {
					runner = "gradle"
				}
			}
		case "flutter":
			if resolved[1] == "test" {
				runner = "flutter"
			}
		}
	}
	if runner != "" {
		if code == 0 && runErr == nil && !interrupted && ctx.Err() == nil {
			if runner == "gradle" {
				completed = testcause.ProvenGradleResults(r.Tree, started)
			} else {
				completed = testcause.ProvenFlutterResults(r.Tree, output)
			}
		}
	} else if !goTest {
		completed = completedTestFiles(r.Tree, resolved)
	}
	// The summary counts tests, so the selected file count is only a lower bound.
	node := testcause.IsNodeTestCommand(resolved)
	if node && completed > testcause.NodePassedTestCount(output) {
		completed = 0
	}
	if completed == 0 && runner == "" {
		if !node {
			proven = testcause.ProvenGoPackages(r.Tree, output)
		}
		if proven == 0 {
			proven = testcause.ProvenNodeTestFiles(r.Tree, resolved, output)
		}
	}
	return testcause.Create(testcause.Measurement{SHA: head, BaseSHA: r.Base, Source: source, Tree: r.Tree, Command: resolved, Environment: environment, Output: output, Code: code, Truncated: errors.Is(runErr, process.ErrOutputLimit), Interrupted: interrupted, Started: started, Finished: finished, CompletedFiles: completed, ProvenPackages: proven}), environment
}

// Count tests selected by understood direct arguments.
func completedTestFiles(tree string, command []string) int {
	if len(command) == 0 {
		return 0
	}
	node := testcause.IsNodeTestCommand(command)
	args := command[1:]
	if node {
		var ok bool
		args, ok = testcause.NodeTestSelection(command)
		if !ok {
			return 0
		}
	}
	count := 0
	seen := map[string]bool{}
	for _, arg := range args {
		patterns := []string{arg}
		if strings.Contains(arg, "{mjs,ts}") {
			patterns = []string{strings.ReplaceAll(arg, "{mjs,ts}", "mjs"), strings.ReplaceAll(arg, "{mjs,ts}", "ts")}
		}
		matches := []string{}
		for _, pattern := range patterns {
			var found []string
			if node {
				found = nodeGlobFiles(tree, pattern)
			} else {
				found, _ = filepath.Glob(filepath.Join(tree, pattern))
			}
			matches = append(matches, found...)
		}
		for _, file := range matches {
			if strings.Contains(filepath.Base(file), ".test.") && !seen[file] {
				seen[file] = true
				count++
			}
		}
	}
	if len(command) > 2 && filepath.Base(command[0]) == "go" && command[1] == "test" {
		for _, arg := range command[2:] {
			if arg == "./..." {
				_ = filepath.WalkDir(tree, func(path string, d os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if d.IsDir() && (d.Name() == ".git" || d.Name() == "vendor") {
						return filepath.SkipDir
					}
					if !d.IsDir() && strings.HasSuffix(d.Name(), "_test.go") {
						count++
					}
					return nil
				})
				break
			}
		}
	}
	return count
}

// nodeGlobFiles expands path segments without leaving the tree. It excludes dot
// entries, node_modules, vendor, and symlinks even when explicitly named by a
// literal path segment; this conservatively limits the files counted as evidence.
// A ** segment consumes zero or more directories.
func nodeGlobFiles(tree, pattern string) []string {
	if tree == "" || filepath.IsAbs(pattern) {
		return nil
	}
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	parts := strings.Split(pattern, "/")
	for _, part := range parts {
		if part == ".." || part == "" {
			return nil
		}
	}
	var matches []string
	var walk func(string, int)
	walk = func(dir string, i int) {
		if i >= len(parts) {
			return
		}
		part := parts[i]
		if part == "**" {
			walk(dir, i+1)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" {
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 {
				continue
			}
			path := filepath.Join(dir, name)
			if part == "**" {
				if entry.IsDir() {
					walk(path, i)
				}
				continue
			}
			matched, err := filepath.Match(part, name)
			if err != nil || !matched {
				continue
			}
			if i == len(parts)-1 {
				if entry.Type().IsRegular() {
					matches = append(matches, path)
				}
			} else if entry.IsDir() {
				walk(path, i+1)
			}
		}
	}
	walk(tree, 0)
	return matches
}

func (e *Executor) classifyTest(repo string, observation testcause.Object) testcause.Object {
	e.observationsMu.Lock()
	defer e.observationsMu.Unlock()
	// Full test baselines survive restart; missing or malformed evidence remains unknown.
	file := filepath.Join(e.Config.RuntimeDir, "control", "test-observations", strings.ReplaceAll(repo, "/", "__")+".json")
	var previous []any
	if raw, err := os.ReadFile(file); err == nil && len(raw) <= 2<<20 {
		_ = json.Unmarshal(raw, &previous)
	}
	report := testcause.Classify(observation, previous)
	kind, _ := report["kind"].(string)
	report["acquisition"] = "保存済みの全テスト証拠を照合しました。基点の追加実行はありません。"
	if kind == "environment" {
		report["acquisition"] = "今回の起動時の権限診断を使いました。基点の追加実行はありません。"
	}
	if observation != nil && observation["source"] == "full-test" && observation["sha"] != nil {
		report["observationStored"] = false
		previous = append([]any{observation}, previous...)
		if len(previous) > 50 {
			previous = previous[:50]
		}
		if raw, err := json.Marshal(previous); err == nil && len(raw) <= 2<<20 {
			if os.MkdirAll(filepath.Dir(file), 0700) == nil {
				report["observationStored"] = atomicfile.Write(file, ".observation-", 0600, false, raw, nil) == nil
			}
		}
	}
	// Include the metadata in the budget, and let Report discard the entire
	// classification when its supporting evidence does not fit.
	metadata := testcause.Object{"acquisition": clip(report["acquisition"].(string), 500)}
	if stored, ok := report["observationStored"]; ok {
		metadata["observationStored"] = stored
	}
	metadataJSON, _ := json.Marshal(metadata)
	bounded := testcause.Report(report, 24*1024-(len(metadataJSON)-2)-1)
	for key, value := range metadata {
		bounded[key] = value
	}
	if raw, err := json.Marshal(bounded); err != nil || len(raw) > 24*1024 {
		bounded = testcause.Classify(nil, nil)
		for key, value := range metadata {
			bounded[key] = value
		}
	}
	return bounded
}

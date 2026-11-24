package testcause

import (
	"encoding/json"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ProvenNodeTestFiles counts the test files that a bare `node --test` (no file
// arguments) runs in tree, using Node's default patterns. The command may call node
// directly or through the package.json test script (`pnpm test`, `npm test`,
// `pnpm run test`). It returns 0 unless output carries the runner's complete summary
// with consistent success counts (see NodePassedTestCount). Commands that name files
// are counted elsewhere and return 0 here.
func ProvenNodeTestFiles(tree string, command []string, output string) int {
	if tree == "" {
		return 0
	}
	args, direct := nodeTestArgs(tree, command)
	if args == nil {
		return 0
	}
	files, test := nodeTestSelection(args, direct)
	if !test || len(files) != 0 {
		return 0
	}
	tests := NodePassedTestCount(output)
	if tests == 0 {
		return 0
	}
	count := 0
	_ = filepath.WalkDir(tree, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == tree {
			return nil
		}
		// Node's default glob skips node_modules and dot entries (.git, .tmp, .github).
		if entry.Name() == "node_modules" || strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() && entry.Type().IsRegular() && nodeDefaultTestFile(tree, path) {
			count++
		}
		return nil
	})
	// Node reports each file as at least one test; fewer tests than files means some
	// files did not run (for example .ts files on a Node without type stripping).
	if count > tests {
		return 0
	}
	return count
}

// IsNodeCommand identifies a directly invoked Node executable.
func IsNodeCommand(command []string) bool {
	if len(command) == 0 {
		return false
	}
	switch filepath.Base(command[0]) {
	case "node", "nodejs", "node.exe":
		return true
	}
	return false
}

// IsNodeTestCommand identifies direct Node commands that require test runner
// evidence. NodeTestSelection validates the arguments, and rejected commands
// cannot fall back to generic file or Go evidence.
func IsNodeTestCommand(command []string) bool {
	if !IsNodeCommand(command) {
		return false
	}
	for _, arg := range command[1:] {
		if arg == "--test" || strings.HasPrefix(arg, "--test=") {
			return true
		}
	}
	return false
}

// NodeTestSelection returns explicit file arguments for a direct Node test run
// whose options permit full-run evidence.
func NodeTestSelection(command []string) ([]string, bool) {
	if !IsNodeTestCommand(command) {
		return nil, false
	}
	return nodeTestSelection(command[1:], true)
}

func nodeTestSelection(args []string, direct bool) ([]string, bool) {
	var files []string
	test := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--" && test && direct:
			return append(files, args[i+1:]...), true
		case arg == "--test":
			test = true
		case strings.HasPrefix(arg, "-"):
			name, value, assigned := strings.Cut(arg, "=")
			switch name {
			case "--test-concurrency", "--test-timeout", "--test-reporter", "--disable-proto", "--allow-fs-read", "--allow-fs-write", "--unhandled-rejections":
				if direct && !assigned && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
					value = args[i]
				}
				if !nodeAllowedFlag(name+"="+value, direct) {
					return nil, false
				}
			case "--test-skip-pattern":
				// Only the Worker's configured command may exclude tests by name.
				if !direct || !assigned || value == "" {
					return nil, false
				}
			default:
				// Reject narrowing flags (including split forms), loaders and custom reporters.
				if !nodeAllowedFlag(arg, direct) {
					return nil, false
				}
			}
		case !test:
			// Node passes arguments after a script name to the script.
			return nil, false
		default:
			files = append(files, arg)
		}
	}
	return files, test
}

// nodeAllowedFlag accepts only flags that cannot narrow, replace or hide the run.
// --test-shard, --test-only, --test-name-pattern, --import, --require and custom
// reporters could run a subset or rewrite the summary.
func nodeAllowedFlag(arg string, direct bool) bool {
	for _, prefix := range []string{"--test-concurrency=", "--test-timeout="} {
		if value, ok := strings.CutPrefix(arg, prefix); ok {
			_, err := strconv.Atoi(value)
			return err == nil
		}
	}
	switch arg {
	case "--test-reporter=spec", "--test-reporter=tap", "--test-reporter=dot":
		return true
	}
	if !direct {
		return false
	}
	switch arg {
	case "--no-warnings", "--frozen-intrinsics", "--disallow-code-generation-from-strings", "--no-global-search-paths", "--no-addons", "--disable-proto=delete", "--disable-proto=throw",
		"--experimental-strip-types", "--experimental-transform-types", "--no-strip-types",
		"--experimental-permission", "--permission", "--allow-child-process", "--allow-worker",
		"--unhandled-rejections=strict", "--unhandled-rejections=throw":
		return true
	}
	if name, value, ok := strings.Cut(arg, "="); ok && value != "" {
		return name == "--allow-fs-read" || name == "--allow-fs-write"
	}
	return false
}

var nodeSummaryLine = regexp.MustCompile(`^(?:ℹ|#) (tests|suites|pass|fail|cancelled|skipped|todo|duration_ms) ([\d.]+)\s*$`)

// NodePassedTestCount returns the test total when all summary counts are complete,
// consistent and successful; otherwise it returns 0.
func NodePassedTestCount(output string) int {
	counts := map[string]int{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if reportedFailure.MatchString(line) {
			return 0
		}
		match := nodeSummaryLine.FindStringSubmatch(line)
		if match == nil || match[1] == "suites" || match[1] == "duration_ms" {
			continue
		}
		if _, seen := counts[match[1]]; seen {
			return 0
		}
		n, err := strconv.Atoi(match[2])
		if err != nil {
			return 0
		}
		counts[match[1]] = n
	}
	for _, key := range []string{"tests", "pass", "fail", "cancelled", "skipped", "todo"} {
		if _, ok := counts[key]; !ok {
			return 0
		}
	}
	if counts["fail"] != 0 || counts["cancelled"] != 0 || counts["pass"] <= 0 {
		return 0
	}
	if counts["pass"]+counts["skipped"]+counts["todo"] != counts["tests"] {
		return 0
	}
	return counts["tests"]
}

// nodeTestArgs returns the node arguments of command and whether they come directly
// from the configured command (true) or from the repository's package.json test script.
func nodeTestArgs(tree string, command []string) ([]string, bool) {
	if len(command) == 0 {
		return nil, false
	}
	name := filepath.Base(command[0])
	if IsNodeCommand(command) {
		return append([]string{}, command[1:]...), true
	}
	if name != "pnpm" && name != "npm" {
		return nil, false
	}
	rest := command[1:]
	if len(rest) == 2 && rest[0] == "run" && rest[1] == "test" {
		rest = rest[1:]
	}
	if len(rest) != 1 || rest[0] != "test" {
		return nil, false
	}
	data, err := readTreeFile(tree, "package.json", 8<<20)
	if err != nil {
		return nil, false
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &manifest) != nil {
		return nil, false
	}
	fields := strings.Fields(manifest.Scripts["test"])
	if len(fields) == 0 || fields[0] != "node" {
		return nil, false
	}
	return fields[1:], false
}

var nodeTestExtensions = map[string]bool{".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".mts": true, ".cts": true}

// nodeDefaultTestFile follows the default patterns of `node --test`:
// **/*.test.*, **/*-test.*, **/*_test.*, **/test-*.*, **/test.* and **/test/**/*.*.
func nodeDefaultTestFile(tree, path string) bool {
	base := filepath.Base(path)
	extension := filepath.Ext(base)
	if !nodeTestExtensions[extension] {
		return false
	}
	stem := strings.TrimSuffix(base, extension)
	if stem == "test" || strings.HasPrefix(stem, "test-") || strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, "-test") || strings.HasSuffix(stem, "_test") {
		return true
	}
	relative, err := filepath.Rel(tree, filepath.Dir(path))
	if err != nil {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		if part == "test" {
			return true
		}
	}
	return false
}

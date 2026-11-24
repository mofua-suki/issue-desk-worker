package testcause

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var todoCommentPattern = regexp.MustCompile(`(?i)\s#\s*TODO\b`)
var goTestBrokenPattern = regexp.MustCompile(`(?m)^(?:panic:|fatal error:|FAIL\s+\S+\s+\[|# )`)
var goTestFailPattern = regexp.MustCompile(`(?m)^FAIL\s*$`)
var errorWordPattern = regexp.MustCompile(`(?i)\b(?:error|errors|interrupted)\b`)
var pytestErrorPattern = regexp.MustCompile(`(?m)^(?:ERROR |!+ Interrupted)`)
var python3Pattern = regexp.MustCompile(`^python3(?:\.\d+)?$`)
var missingToolDiagnosticPattern = regexp.MustCompile(`(?im)^(?:# )?(?:Error \[(?:EACCES|EPERM|ERR_MODULE_NOT_FOUND)\]:|Error: (?:(?:EACCES|EPERM|ERR_MODULE_NOT_FOUND)\b|spawn\s+\S+\s+ENOENT\b|command not found\b)|(?:/\S+/)?(?:ba)?sh: [^\r\n]*command not found)`)
var permissionErrorPattern = regexp.MustCompile(`(?:EPERM|EACCES):[^\n]*, ([A-Za-z_]{1,100}) '([^']+)'`)
var assertionFailurePattern = regexp.MustCompile(`(?m)^\s*not ok\b|^\s*✖ |^\s*FAILED |failing tests:|AssertionError|ERR_ASSERTION|SyntaxError|TypeError|ReferenceError|error TS\d+`)

// Measurement retains execution conditions, never the raw test output.
type Measurement struct {
	SHA, BaseSHA, Source, Tree string
	Command                    []string
	Environment                Object
	Output                     string
	Code                       int
	CompletedFiles             int
	// ProvenPackages counts Go test packages of Tree that the output reports as passed
	// (see ProvenGoPackages), or the test files of a bare `node --test` whose complete
	// summary passed (see ProvenNodeTestFiles); it is set only when the command names
	// no test files.
	ProvenPackages         int
	Truncated, Interrupted bool
	Started, Finished      time.Time
}

func condition(v any) bool {
	switch value := v.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case bool:
		return true
	case float64:
		return !math.IsInf(value, 0) && !math.IsNaN(value)
	case int, int64:
		return true
	case []any:
		if len(value) == 0 {
			return false
		}
		for _, x := range value {
			if !condition(x) {
				return false
			}
		}
		return true
	case map[string]any:
		if len(value) == 0 {
			return false
		}
		for _, x := range value {
			if !condition(x) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
func digest(v any) string { h := sha256.Sum256(encoding(v)); return hex.EncodeToString(h[:]) }

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
var summary = regexp.MustCompile(`^[#ℹ] (tests|pass|fail|cancelled|skipped|todo) (\d+)\s*$`)
var tap = regexp.MustCompile(`^( *)not ok \d+ - (.+?)(?:\s+#.*)?$`)
var spec = regexp.MustCompile(`^✖ (.+?)(?: \([\d.]+m?s\))?$`)
var location = regexp.MustCompile(`(?:shared/test|server/test|client/test|test)/[^\s'":]+\.test\.(?:mjs|ts)(?:[:'"\s]|$)`)
var duration = regexp.MustCompile(`duration_ms: [\d.]+`)
var specDuration = regexp.MustCompile(`(?m)^(✖ .+?) \([\d.]+m?s\)$`)
var fileFailure = regexp.MustCompile(`(?m)^\s*(?:error: 'test failed'|exitCode: |'test failed')`)

func indent(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }
func trimBlock(lines []string) string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \r\t")
	}
	return strings.Join(lines, "\n")
}
func parts(output string) ([]string, string) {
	lines := strings.Split(output, "\n")
	blocks := []string{}
	for i := 0; i < len(lines); i++ {
		m := tap.FindStringSubmatch(lines[i])
		if m == nil || todoCommentPattern.MatchString(lines[i]) {
			continue
		}
		block := []string{lines[i]}
		if i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "---" && indent(lines[i+1]) > len(m[1]) {
			yaml := indent(lines[i+1])
			i++
			block = append(block, lines[i])
			for i+1 < len(lines) {
				line := lines[i+1]
				if strings.TrimSpace(line) != "" && indent(line) < yaml {
					break
				}
				i++
				block = append(block, line)
				if strings.TrimSpace(line) == "..." && indent(line) == yaml {
					break
				}
			}
		}
		blocks = append(blocks, trimBlock(block))
	}
	if len(blocks) > 0 {
		return blocks, "tap"
	}
	at := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "✖ failing tests:" {
			at = i
		}
	}
	if at < 0 {
		return blocks, "spec"
	}
	var current []string
	for _, line := range lines[at+1:] {
		located := len(current) == 1 && strings.HasPrefix(current[0], "test at ")
		if strings.HasPrefix(line, "test at ") || strings.HasPrefix(line, "✖ ") && !located {
			if current != nil {
				blocks = append(blocks, trimBlock(current))
			}
			current = []string{line}
		} else if current == nil {
			if strings.TrimSpace(line) != "" {
				break
			}
		} else if strings.TrimSpace(line) != "" && indent(line) == 0 && !located {
			break
		} else {
			current = append(current, line)
		}
	}
	if current != nil {
		blocks = append(blocks, trimBlock(current))
	}
	return blocks, "spec"
}

// foreignFailures recognizes only complete, conventional go test and pytest reports.
// Unrecognized or ambiguous output is deliberately not used as evidence.
var goResult = regexp.MustCompile(`(?m)^(?:ok|FAIL|\?)\s+\S+\s+(?:[\d.]+s(?:\s+\(cached\))?|\[.*\]|\(cached\))\s*$`)
var goFail = regexp.MustCompile(`^\s*--- FAIL: (\S+) \([\d.]+s\)\s*$`)
var goPackage = regexp.MustCompile(`^FAIL\s+(\S+)\s+([\d.]+s|\(cached\))\s*$`)
var goTime = regexp.MustCompile(`\([\d.]+s\)`)
var pytestLine = regexp.MustCompile(`^FAILED (\S+::\S+)(?:\s+-\s+.*)?$`)
var pytestEnd = regexp.MustCompile(`^=+\s+(.+?)\s+in [\d.]+s(?:\s+\([^()]*\))?\s+=+$`)
var pytestCount = regexp.MustCompile(`(?:^|, )([0-9]+) failed(?:,|$)`)
var pytestSection = regexp.MustCompile(`^=+\s+(.+?)\s+=+$`)
var pytestFailureHeader = regexp.MustCompile(`^_+\s+(.+?)\s+_+$`)
var pytestFrameSeparator = regexp.MustCompile(`^_(?: _)+\s*$`)
var pytestDiagnostic = regexp.MustCompile(`(?m)^E\s+\S`)
var pytestLocation = regexp.MustCompile(`(?m)^\S[^\n]*:\d+:\s+\S`)

func foreignFailures(output string, truncated bool) (string, []any, []any, any, bool) {
	lines := strings.Split(output, "\n")
	goFound := goResult.MatchString(output)
	pyFound := strings.Contains(output, "test session starts") || strings.Contains(output, "short test summary info")
	nodeFound := false
	for _, line := range lines {
		if summary.MatchString(line) {
			nodeFound = true
		}
	}
	if (goFound && pyFound) || (nodeFound && (goFound || pyFound)) {
		return "ambiguous", []any{}, []any{}, nil, false
	}
	names, failures := []any{}, []any{}
	seen := map[string]bool{}
	valid := !truncated
	add := func(name, detail string) {
		if name == "" || length(name) > 1000 || seen[name] {
			valid = false
			return
		}
		seen[name] = true
		names = append(names, name)
		failures = append(failures, Object{"test": name, "fingerprint": digest(detail)})
	}
	if goFound {
		pending := []string{}
		block := []string{}
		failLines, packages := 0, 0
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "--- FAIL:") && !goFail.MatchString(line) {
				valid = false
			}
			if match := goFail.FindStringSubmatch(line); match != nil {
				pending = append(pending, match[1])
				failLines++
			}
			if match := goPackage.FindStringSubmatch(line); match != nil {
				packages++
				if len(pending) == 0 {
					valid = false
				}
				detail := goTime.ReplaceAllString(trimBlock(block), "(measured)")
				for _, name := range pending {
					add(match[1]+" :: "+name, detail)
				}
				pending, block = nil, nil
				continue
			}
			if !goResult.MatchString(line) && strings.TrimSpace(line) != "FAIL" {
				block = append(block, line)
			}
		}
		if len(pending) != 0 || packages == 0 || failLines != len(names) || goTestBrokenPattern.MatchString(output) || !goTestFailPattern.MatchString(output) {
			valid = false
		}
		return "go", names, failures, failLines, valid && failLines > 0 && len(names) <= 200
	}
	if pyFound {
		linesCount, totals, total := 0, 0, 0
		bodies := map[string]string{}
		inFailures := false
		header := ""
		var body []string
		flush := func() {
			if header != "" {
				detail := trimBlock(body)
				if _, exists := bodies[header]; exists || !pytestDiagnostic.MatchString(detail) || !pytestLocation.MatchString(detail) {
					valid = false
				}
				bodies[header] = detail
			}
			header, body = "", nil
		}
		for _, line := range lines {
			if section := pytestSection.FindStringSubmatch(line); section != nil {
				if inFailures {
					flush()
				}
				inFailures = section[1] == "FAILURES"
				continue
			}
			if !inFailures {
				continue
			}
			if pytestFrameSeparator.MatchString(line) {
				// A multi-frame traceback separator belongs to the current failure's body.
				if header != "" {
					body = append(body, line)
				}
			} else if match := pytestFailureHeader.FindStringSubmatch(line); match != nil {
				flush()
				header = match[1]
			} else if header != "" {
				body = append(body, line)
			}
		}
		if inFailures {
			flush()
			valid = false
		}
		used := map[string]bool{}
		for _, line := range lines {
			if match := pytestLine.FindStringSubmatch(line); match != nil {
				linesCount++
				parts := strings.Split(match[1], "::")
				key := strings.Join(parts[1:], ".")
				detail, ok := bodies[key]
				if !ok || used[key] {
					valid = false
				} else {
					used[key] = true
					add(match[1], detail)
				}
			} else if strings.HasPrefix(line, "FAILED ") {
				valid = false
			}
			if match := pytestEnd.FindStringSubmatch(line); match != nil {
				totals++
				if count := pytestCount.FindStringSubmatch(match[1]); count != nil {
					total, _ = strconv.Atoi(count[1])
				} else {
					valid = false
				}
				if errorWordPattern.MatchString(match[1]) {
					valid = false
				}
			}
		}
		if pytestErrorPattern.MatchString(output) {
			valid = false
		}
		return "python", names, failures, total, valid && totals == 1 && total > 0 && linesCount == total && len(names) == total && len(bodies) == total && total <= 200
	}
	return "", nil, nil, nil, false
}

// directPytest accepts only direct runner invocations, never a shell printing a summary.
func directPytest(command []string) bool {
	if len(command) == 0 {
		return false
	}
	name := filepath.Base(command[0])
	return name == "pytest" || ((name == "python" || python3Pattern.MatchString(name)) && len(command) >= 3 && command[1] == "-m" && command[2] == "pytest")
}

var pytestPassSummary = regexp.MustCompile(`^(?:=+\s+)?(\d+) passed(?:, \d+ (?:skipped|xfailed|xpassed|deselected|warnings?))* in \d+(?:\.\d+)?s(?: \(\d+:\d{2}:\d{2}\))?(?:\s+=+)?$`)
var pytestBadReport = regexp.MustCompile(`(?im)^(?:\s*(?:FAILED|ERROR|FAILURES|ERRORS)\b|\s*!+\s*Interrupted\b)|\b\d+ (?:failed|errors?)\b`)

func pytestPassed(output string) bool {
	if pytestBadReport.MatchString(output) {
		return false
	}
	count := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if m := pytestPassSummary.FindStringSubmatch(line); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil || n <= 0 {
				return false
			}
			count++
		}
	}
	return count == 1
}

// Create derives diagnostics from complete measured output. Unsupported output remains incomplete.
var toolWarningLine = regexp.MustCompile(`(?i)^\s*(?:\[WARN\]|(?:npm\s+)?WARN\b)`)

// WithoutToolWarnings drops package-manager warning lines. A warning such as pnpm failing to read
// ~/.npmrc did not stop the run, so its path is not the cause of a failed test run.
func WithoutToolWarnings(output string) string {
	lines := strings.Split(output, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if !toolWarningLine.MatchString(line) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// summaryCounts reads the runner's summary lines (tests, pass, fail, ...).
// unique is false when a summary key repeats; contradictoryPass is a nonzero
// fail or cancelled count.
func summaryCounts(output string) (counts map[string]int, unique, contradictoryPass bool) {
	counts = map[string]int{}
	unique = true
	for _, line := range strings.Split(output, "\n") {
		if match := summary.FindStringSubmatch(line); match != nil {
			if (match[1] == "fail" || match[1] == "cancelled") && strings.Trim(match[2], "0") != "" {
				contradictoryPass = true
			}
			if _, ok := counts[match[1]]; ok {
				unique = false
			}
			counts[match[1]], _ = strconv.Atoi(match[2])
		}
	}
	return counts, unique, contradictoryPass
}

// failedTests names each failure block as "file :: test" with a fingerprint
// that ignores durations. parsed is false when a block cannot be named.
func failedTests(blocks []string, format string) (names, failures []any, parsed bool) {
	names, failures, parsed = []any{}, []any{}, true
	for _, block := range blocks {
		if fileFailure.MatchString(block) {
			parsed = false
			break
		}
		var loc string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "test at ") || strings.HasPrefix(strings.TrimLeft(line, " "), "location: ") {
				loc = strings.ReplaceAll(line, "\\", "/")
				break
			}
		}
		file := location.FindString(loc)
		file = strings.TrimRight(file, ":'\" \t")
		name := ""
		if match := tap.FindStringSubmatch(strings.Split(block, "\n")[0]); match != nil {
			name = match[2]
		} else {
			for _, line := range strings.Split(block, "\n") {
				if match := spec.FindStringSubmatch(line); match != nil {
					name = match[1]
					break
				}
			}
		}
		if file == "" || name == "" {
			parsed = false
			continue
		}
		name = file + " :: " + strings.TrimSpace(name)
		if length(name) > 1000 {
			parsed = false
			continue
		}
		names = append(names, name)
		normalized := duration.ReplaceAllString(block, "duration_ms: measured")
		if format == "spec" {
			normalized = specDuration.ReplaceAllString(block, "$1 (measured)")
		}
		failures = append(failures, Object{"test": name, "fingerprint": digest(normalized)})
	}
	return names, failures, parsed
}

// conditionsComplete reports whether the command and the environment it ran
// in (the runtime version for its language and the time zone) are all known.
func conditionsComplete(m Measurement, foreign string, pytestDirect bool) bool {
	runtimeKey := "node"
	if foreign == "go" || len(m.Command) > 1 && filepath.Base(m.Command[0]) == "go" && m.Command[1] == "test" {
		runtimeKey = "go"
	} else if foreign == "python" || pytestDirect {
		runtimeKey = "python"
	}
	conditions := len(m.Command) > 0 && condition(m.Environment) && strings.TrimSpace(str(m.Environment[runtimeKey])) != "" && strings.TrimSpace(str(m.Environment["tz"])) != ""
	for _, s := range m.Command {
		conditions = conditions && strings.TrimSpace(s) != ""
	}
	return conditions
}

// addPermissionEvidence records an outside-path permission failure. The
// caller has checked the conditions and that no test failed.
func addPermissionEvidence(observation Object, m Measurement, output string) {
	// Permission diagnostics require no test failure, a measured nonzero exit and an outside path.
	permission := permissionErrorPattern.FindStringSubmatch(WithoutToolWarnings(output))
	if m.Truncated || m.Interrupted || m.Code != 1 || len(permission) != 3 || assertionFailurePattern.MatchString(output) {
		return
	}
	code := "EACCES"
	if strings.HasPrefix(permission[0], "EPERM") {
		code = "EPERM"
	}
	p := copyFields(observation, "sha", "baseSha", "command", "environment", "at", "source")
	p["code"], p["syscall"], p["path"], p["tree"], p["run"] = code, permission[1], permission[2], m.Tree, Object{"code": 1, "truncated": false, "killed": nil}
	observation["environmentFailure"] = "permission"
	observation["permissionEvidence"] = p
	observation["complete"] = true
}

// completedFiles is the positive count of completed test files or packages
// that pass evidence may claim; 0 means none is proven.
func completedFiles(m Measurement, output string, pytestDirect, intact bool) int {
	completed := m.CompletedFiles
	if pytestDirect {
		// The persisted files field is a positive completion marker, not a pytest test count.
		completed = 0
		if pytestPassed(output) && intact {
			completed = 1
		}
	}
	// A wrapper run (scripts/verify.sh) counts only packages that exist in the tree and
	// that the output reports as passed, with no failure anywhere; a printed summary alone
	// is not proof that tests ran.
	if !pytestDirect && completed == 0 && m.ProvenPackages > 0 && reportedPasses(output) > 0 {
		completed = m.ProvenPackages
	}
	return completed
}

func Create(m Measurement) Object {
	output := strings.ReplaceAll(ansi.ReplaceAllString(m.Output, ""), "\r\n", "\n")
	counts, unique, contradictoryPass := summaryCounts(output)
	blocks, format := parts(output)
	names, failures, parsed := failedTests(blocks, format)
	sum := counts["pass"] + counts["fail"] + counts["cancelled"] + counts["skipped"] + counts["todo"]
	_, hasTests := counts["tests"]
	_, hasPass := counts["pass"]
	_, hasFail := counts["fail"]
	_, hasCancelled := counts["cancelled"]
	diagnostic := missingToolDiagnosticPattern.MatchString(output)
	intact := !m.Truncated && !m.Interrupted && !diagnostic
	complete := intact && m.Code == 1 && unique && hasTests && hasPass && hasFail && hasCancelled && counts["tests"] == sum && counts["fail"] > 0 && counts["cancelled"] == 0 && parsed && len(names) == counts["fail"] && len(names) <= 200
	var total any
	if hasFail && unique && !m.Truncated {
		total = counts["fail"]
	}
	foreign, foreignNames, foreignDetails, foreignTotal, foreignComplete := foreignFailures(output, m.Truncated || m.Interrupted)
	if foreign != "" {
		names, failures, total = foreignNames, foreignDetails, foreignTotal
		complete = foreignComplete && intact && m.Code == 1
	}
	evidence := Object{"version": 1, "names": names, "total": total, "complete": complete, "timeout": nil}
	if len(encoding(evidence)) > 16000 {
		evidence = Object{"version": 1, "names": []any{}, "total": total, "complete": false, "timeout": nil}
		complete = false
		failures = []any{}
	}
	pytestDirect := directPytest(m.Command)
	conditions := conditionsComplete(m, foreign, pytestDirect)
	var command, environment any
	if conditions {
		command = digest(m.Command)
		environment = digest(m.Environment)
	}
	var target, base any
	if sha.MatchString(m.SHA) {
		target = m.SHA
	}
	if sha.MatchString(m.BaseSHA) {
		base = m.BaseSHA
	}
	source := m.Source
	if source == "" {
		source = "fixed-test"
	}
	at := m.Finished.UTC().Format("2006-01-02T15:04:05.000Z")
	observation := Object{"sha": target, "baseSha": base, "at": at, "source": source, "command": command, "environment": environment, "conditionsComplete": conditions, "complete": complete && conditions, "passed": m.Code == 0 && !m.Interrupted && !m.Truncated && !contradictoryPass, "environmentFailure": nil, "permissionEvidence": nil, "passEvidence": nil, "failureEvidence": nil, "failures": failures}
	if complete && conditions {
		observation["failureEvidence"] = Object{"evidence": evidence, "run": Object{"code": 1, "truncated": false, "killed": nil}}
	}
	if conditions && len(names) == 0 {
		addPermissionEvidence(observation, m, output)
	}
	completed := completedFiles(m, output, pytestDirect, intact)
	if observation["passed"] == true && intact && (source == "full-test" || source == "fixed-test") && conditions && completed > 0 && target != nil && base != nil && len(blocks) == 0 {
		record := Object{"status": "passed", "sha": target, "startedAt": m.Started.UTC().Format("2006-01-02T15:04:05.000Z"), "finishedAt": at, "files": completed, "ms": m.Finished.Sub(m.Started).Milliseconds(), "failedFiles": []any{}, "excerpt": passExcerpt(output), "failures": Object{"names": []any{}, "complete": true}, "execution": Object{"command": command, "environment": environment, "baseSha": base}}
		proof := copyFields(observation, "sha", "baseSha", "command", "environment", "at", "source")
		proof["record"], proof["run"] = record, Object{"code": 0, "truncated": false, "killed": nil}
		observation["passEvidence"], observation["complete"] = proof, true
	}
	// Normalize through the persisted validator so generated and recovered evidence have identical trust.
	raw := encoding(observation)
	var normalized any
	_ = json.Unmarshal(raw, &normalized)
	clean := cleanObservation(normalized)
	addPassEvidenceMissing(clean, conditions, completed, target != nil, base != nil, intact, len(blocks) > 0 || contradictoryPass, source)
	return clean
}

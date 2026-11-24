package testcause

import (
	"regexp"
	"strconv"
	"strings"
)

var goPass = regexp.MustCompile(`^ok\s+\S+\s+(?:[\d.]+s|\(cached\))`)
var pytestSummary = regexp.MustCompile(`^=+ (.+?) =+$`)
var pytestTally = regexp.MustCompile(`(\d+) (passed|failed|errors?|xpassed|xfailed|skipped|deselected|warnings?)\b`)
var reportedFailure = regexp.MustCompile(`^(?:--- FAIL|FAIL\b|not ok \d+)`)

// reportedPasses counts the passed tests stated by the test runners' own summaries.
// It is used when the command names no test files (a wrapper such as scripts/verify.sh,
// node --test without file arguments, or pytest), so the completed count cannot be
// derived from the command. Any reported failure, cancellation or error yields 0.
func reportedPasses(output string) int {
	total := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, " \t")
		if reportedFailure.MatchString(line) {
			return 0
		}
		if match := summary.FindStringSubmatch(line); match != nil {
			n, _ := strconv.Atoi(match[2])
			if (match[1] == "fail" || match[1] == "cancelled") && n > 0 {
				return 0
			}
			if match[1] == "pass" {
				total += n
			}
			continue
		}
		if goPass.MatchString(line) && !strings.Contains(line, "[no tests to run]") {
			total++
			continue
		}
		if match := pytestSummary.FindStringSubmatch(line); match != nil && strings.Contains(match[1], " passed") {
			for _, count := range pytestTally.FindAllStringSubmatch(match[1], -1) {
				n, _ := strconv.Atoi(count[1])
				switch count[2] {
				case "passed":
					total += n
				case "failed", "error", "errors":
					if n > 0 {
						return 0
					}
				}
			}
		}
	}
	return total
}

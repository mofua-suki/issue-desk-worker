package execution

import (
	"context"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/process"
	"issue-desk-worker/internal/testcause"
	"path/filepath"
	"regexp"
	"strings"
)

type testEnvironmentError struct{}

func (testEnvironmentError) Error() string {
	return "作業ツリー外のアクセス権限によりテストを実行できません"
}

var permissionDiagnostic = regexp.MustCompile(`\b(?:EPERM|EACCES)\b:[^,\r\n]*,\s*([A-Za-z_]+)\s+'([^'\r\n]+)'`)
var passedDiagnostic = regexp.MustCompile(`(?m)^\s*ok [0-9]+\b|^\s*ok[ \t]+\S+[ \t]+(?:[0-9.]+s|\(cached\))|^\s*[#ℹ] pass [1-9]|^\s*✔ |\b[1-9][0-9]* passed\b`)

// Preserve the live test-outcome gate independently from commit-attributed cause evidence.
func outsideTestPermission(out process.Result, tree string) bool {
	if out.Code == 0 || out.Interrupted {
		return false
	}
	text := out.Out + "\n" + out.Err
	if passedDiagnostic.MatchString(text) {
		return false
	}
	for _, match := range permissionDiagnostic.FindAllStringSubmatch(testcause.WithoutToolWarnings(text), -1) {
		target := match[2]
		if !filepath.IsAbs(target) {
			target = filepath.Join(tree, target)
		}
		rel, err := filepath.Rel(filepath.Clean(tree), filepath.Clean(target))
		if err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)) {
			return true
		}
	}
	return false
}
func (e *Executor) measuredTest(ctx context.Context, repo Repository, r *journal.Record, source string, prepare ...func() error) (TestResult, error) {
	result, err := e.runMeasuredTest(ctx, repo, r, source, prepare...)
	e.holdsMu.Lock()
	if e.holds.Environment != nil {
		hold := *e.holds.Environment
		result.environmentHold = &hold
	}
	e.holdsMu.Unlock()
	// The host hold also comes from AI calls and other jobs. Only a refusal seen in this run makes the test unavailable.
	if !result.Passed && result.accessDenied && result.environmentHold != nil {
		result.Unavailable = true
		result.countableFailure = false
	}
	return result, err
}

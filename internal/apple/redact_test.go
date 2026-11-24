package apple

import (
	"errors"
	"issue-desk-worker/internal/process"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/redact"
)

// Apple runnerの失敗の記録は、資格情報を伏せてから書く。
func TestFailureLogHidesCredentials(t *testing.T) {
	log := appleFailureLog()
	var out strings.Builder
	log.Out = &out
	log.Observe(errors.New("rejected idk_" + strings.Repeat("a", 43)))
	if strings.Contains(out.String(), strings.Repeat("a", 43)) || !strings.Contains(out.String(), redact.Mark) {
		t.Fatalf("資格情報が残りました: %q", out.String())
	}
}

// ネイティブテストの結果の文も、決まった形式の資格情報を伏せてから送る。
func TestDiagnosticHidesKnownCredentials(t *testing.T) {
	a := &Runner{}
	got := a.diagnostic(process.Result{Err: "token sk-" + strings.Repeat("a", 30)}, nil)
	if strings.Contains(got, strings.Repeat("a", 30)) || !strings.Contains(got, redact.Mark) {
		t.Fatalf("資格情報が残りました: %s", got)
	}
}

// 実行ごとに作る一時領域の名前は、起動時の掃除の型に当たる。
func TestTempPatternsMatchPrefixes(t *testing.T) {
	for _, prefix := range []string{probePrefix, checkoutPrefix, testHomePrefix} {
		matched := false
		for _, pattern := range tempPatterns() {
			if ok, _ := filepath.Match(pattern, prefix+"123"); ok {
				matched = true
			}
		}
		if !matched {
			t.Errorf("%sが掃除されません", prefix)
		}
	}
}

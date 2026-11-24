package execution

import (
	"context"
	"strings"
	"testing"
)

// 実装PRの本文にする実装AIの要約は、資格情報を伏せてから送る。
func TestPullBodyHidesCredentialsInSummary(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.secret = "機能を実装"
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || !f.pull {
		t.Fatal(state, err)
	}
	if strings.Contains(f.Body, "機能を実装") || !strings.Contains(f.Body, redactedMark) || !strings.Contains(f.Body, "Closes #7") {
		t.Fatalf("PRの本文に資格情報が残りました: %q", f.Body)
	}
}

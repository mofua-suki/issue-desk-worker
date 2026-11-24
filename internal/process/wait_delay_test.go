package process

import (
	"context"
	"testing"
)

// 本体が終わった後に子孫が出力を開いたままでも、起動の失敗ではなく本体の終了として返す。
func TestDescendantHoldingOutputIsNotLaunchFailure(t *testing.T) {
	for _, tc := range []struct {
		script string
		code   int
	}{
		{"sleep 5 & echo ok", 0},
		{"sleep 5 & echo ok; exit 3", 3},
	} {
		result, err := Run(context.Background(), "/bin/sh", []string{"-c", tc.script}, Options{})
		if err != nil || result.Code != tc.code || result.Out != "ok\n" {
			t.Fatalf("%s: %+v %v", tc.script, result, err)
		}
	}
}

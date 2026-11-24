package providers

import (
	"strings"
	"testing"

	"issue-desk-worker/internal/process"
)

func TestExitMessageIncludesCodeAndTail(t *testing.T) {
	got := exitMessage(process.Result{Code: 2, Err: "error: unknown model\n  token abcdefghijklmnopqrstuvwxyz0123456789ABCD\n"})
	if !strings.Contains(got, "終了コード2") || !strings.Contains(got, "unknown model") {
		t.Fatalf("終了コードと出力の末尾を載せます: %q", got)
	}
	if strings.Contains(got, "abcdefghijklmnopqrstuvwxyz0123456789ABCD") {
		t.Fatalf("鍵に見える長い文字列は伏せます: %q", got)
	}
	if exitMessage(process.Result{Code: 1}) != "AIが非ゼロで終了しました（終了コード1）" {
		t.Fatal("出力が無いときは終了コードだけです")
	}
}

func TestExitMessageDropsStaleRolloutNoise(t *testing.T) {
	got := exitMessage(process.Result{Code: 1, Err: "error: model not supported\n2026-11-02T09:02:40Z ERROR codex_rollout::list: state db returned stale rollout path for thread x\n"})
	if strings.Contains(got, "stale rollout path") || !strings.Contains(got, "model not supported") {
		t.Fatalf("雑音を除き、本当のエラーを残します: %q", got)
	}
}

func TestExitMessageReadsErrorEventsFromStdout(t *testing.T) {
	got := exitMessage(process.Result{Code: 1, Out: "{\"type\":\"thread.started\"}\n{\"type\":\"error\",\"message\":\"model gpt-x is not available\"}\n"})
	if !strings.Contains(got, "model gpt-x is not available") || strings.Contains(got, "thread.started") {
		t.Fatalf("標準出力のエラーイベントだけを拾います: %q", got)
	}
}

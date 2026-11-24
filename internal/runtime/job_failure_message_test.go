package runtime

import (
	"errors"
	"strings"
	"testing"

	"issue-desk-worker/internal/cause"
)

// 停止の文は、受付の固定の文に付いた要求の型や失敗の種類を残し、それ以外の文は固定の文に置き換える。
func TestSchedulerStopMessageKeepsCallSuffix(t *testing.T) {
	for in, want := range map[error]string{
		errors.New("受付APIの転送を拒否しました（GET /api/v1/jobs/{n}/state）"):               "受付APIの転送を拒否しました（GET /api/v1/jobs/{n}/state）",
		cause.New("受付APIのJSON応答が不正です（POST /api/v1/jobs/next）", errors.New("x")): "受付APIのJSON応答が不正です（POST /api/v1/jobs/next）",
		errors.New("受付APIの証明書を検証できません"):                                         "受付APIの証明書を検証できません",
		errors.New("受付APIの転送を拒否しました: https://secret.example/"):                  "受付の仕事取得または配布の確認に失敗しました",
		errors.New("受付APIの転送を拒否しました（https://secret.example/）"):                  "受付の仕事取得または配布の確認に失敗しました",
	} {
		got := (&schedulerStopError{cause: in, id: 1, reason: "x"}).message()
		if !strings.HasPrefix(got, want) || strings.Contains(got, "secret") {
			t.Fatalf("%v: %q", in, got)
		}
	}
}

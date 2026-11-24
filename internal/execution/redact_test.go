package execution

import (
	"errors"
	"os"
	"strings"
	"testing"

	"issue-desk-worker/internal/process"
)

// 伏せ字は1か所にまとめ、Giteaトークンそのものと既知の形式をどの経路でも同じ印で伏せる。
func TestRedactHidesGiteaTokenAndKnownFormats(t *testing.T) {
	// 秘密の検査に掛からないよう、トークン様の値は実行時に組み立てる。
	secret := "fixture" + strings.Repeat("0", 33)
	e := &Executor{secret: secret}
	for _, value := range []string{
		secret,
		"idk_" + strings.Repeat("a", 24),
		"idh_short",
		"x" + "idh_" + strings.Repeat("b", 24),
		"sk-ant-" + strings.Repeat("c", 8),
		"sk-" + strings.Repeat("d", 24),
		"ghp_" + strings.Repeat("e", 24),
	} {
		if got := e.redact("before " + value + " after"); strings.Contains(got, value) || !strings.Contains(got, redactedMark) {
			t.Fatalf("%q を伏せていません: %q", value, got)
		}
	}
	if got := e.redact("ordinary sk-short gh text"); got != "ordinary sk-short gh text" {
		t.Fatalf("普通の文を変えました: %q", got)
	}
	if got := e.redact("go test issue-desk-worker-execution-test-abcdefgh"); strings.Contains(got, redactedMark) {
		t.Fatalf("ハイフンでつながった名前を伏せました: %q", got)
	}
	var none *Executor
	if got := none.redact("idk_" + strings.Repeat("a", 24)); got != redactedMark {
		t.Fatalf("Executorなしで既知の形式を伏せていません: %q", got)
	}
}

// 以前は既知の形式だけを伏せていた経路（実行の失敗の抜粋・環境の保留理由）も、Giteaトークンを伏せる。
func TestRedactCoversFormerPatternOnlyPaths(t *testing.T) {
	secret := "fixture" + strings.Repeat("1", 33)
	e := &Executor{secret: secret}
	detail := e.commandFailureDetail("失敗", process.Result{Code: 1, Out: "token=" + secret})
	if strings.Contains(detail, secret) {
		t.Fatalf("失敗の抜粋にトークンが残りました: %q", detail)
	}
	e.Config.RuntimeDir = t.TempDir()
	if err := e.recordEnvironmentHold(&os.PathError{Op: "open", Path: "/state/" + secret, Err: os.ErrPermission}); err != nil {
		t.Fatal(err)
	}
	if e.holds.Environment == nil || strings.Contains(e.holds.Environment.Reason, secret) {
		t.Fatalf("保留理由にトークンが残りました: %+v", e.holds.Environment)
	}
}

// 公開前の検査は、Giteaトークンと20文字以上の資格情報だけで断り、短い例（idh_short など）では断らない。
func TestContainsCredentialRefusesOnlyWholeCredentials(t *testing.T) {
	secret := "fixture" + strings.Repeat("2", 33)
	e := &Executor{secret: secret}
	for _, value := range []string{secret, "idk_" + strings.Repeat("a", 24), "sk-" + strings.Repeat("d", 24), "ghp_" + strings.Repeat("e", 24)} {
		if !e.containsCredential("本文 " + value) {
			t.Fatalf("%q で断りません", value)
		}
	}
	for _, text := range []string{"トークンは idh_short の形です", "sk-ant-xxx の形", "issue-desk-worker-execution-test-abcdefgh"} {
		if e.containsCredential(text) {
			t.Fatalf("%q で断りました", text)
		}
	}
}

// 命令の取得の失敗の記録は、資格情報を伏せてから書く。
func TestFailureLogsHideCredentials(t *testing.T) {
	log := commandFailureLog()
	var out strings.Builder
	log.Out = &out
	log.Observe(errors.New("rejected idk_" + strings.Repeat("a", 43)))
	if strings.Contains(out.String(), strings.Repeat("a", 43)) || !strings.Contains(out.String(), redactedMark) {
		t.Fatalf("資格情報が残りました: %q", out.String())
	}
}

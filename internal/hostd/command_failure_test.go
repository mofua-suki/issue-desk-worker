package hostd

import (
	"errors"
	"issue-desk-worker/internal/process"
	"strings"
	"testing"
)

func TestCommandFailureLineKeepsStderrTail(t *testing.T) {
	stderr := strings.Repeat("x", 3000) + "\nError response from daemon: pull access denied (synthetic-password)\n"
	line := commandFailureLine("bash", []string{"/client/update-linux.sh", "/client", "aaaaaaaaaaaa"}, "synthetic-password\n", process.Result{Code: 1, Err: stderr}, nil)
	for _, want := range []string{"bash update-linux.sh", "終了コード1", "pull access denied", "[REDACTED]", "…"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q: %s", want, line)
		}
	}
	if strings.Contains(line, "synthetic-password") {
		t.Fatalf("input leaked: %s", line)
	}
	if len(line) > commandStderrLimit+200 {
		t.Fatalf("not truncated: %d", len(line))
	}
}

func TestCommandFailureLineWithoutStderr(t *testing.T) {
	line := commandFailureLine("docker", []string{"compose", "config"}, "", process.Result{Interrupted: true}, errors.New("起動できません"))
	if !strings.Contains(line, "docker compose") || !strings.Contains(line, "時間切れ") || !strings.Contains(line, "起動できません") {
		t.Fatal(line)
	}
	if line := commandFailureLine("docker", nil, "", process.Result{Code: 2}, nil); !strings.Contains(line, "標準エラーなし") {
		t.Fatal(line)
	}
}

// 標準エラーに出たURLの資格情報は、記録の前に伏せる。切り詰めの境目にあっても一部を残さない。
func TestCommandFailureLineHidesURLCredentials(t *testing.T) {
	stderr := "fatal: unable to access 'https://deploy:s3cr@t-pass@registry.invalid/v2/': 401\n"
	line := commandFailureLine("docker", []string{"pull"}, "", process.Result{Code: 1, Err: stderr}, nil)
	if strings.Contains(line, "s3cr") || strings.Contains(line, "deploy") || !strings.Contains(line, "https://[REDACTED]@registry.invalid") {
		t.Fatalf("資格情報が残りました: %s", line)
	}
}

func TestCommandFailureLineHidesSecretAtCut(t *testing.T) {
	secret := "synthetic-registry-password"
	// 末尾から上限の位置が秘密の途中に当たるように置く。
	stderr := "x" + secret + strings.Repeat("y", commandStderrLimit-len(secret)/2)
	line := commandFailureLine("docker", []string{"login"}, secret+"\n", process.Result{Code: 1, Err: stderr}, nil)
	if strings.Contains(line, secret[len(secret)-8:]) {
		t.Fatalf("切り詰めの境目で秘密の一部が残りました: %s", line)
	}
}

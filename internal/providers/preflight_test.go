package providers

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPreflightRequiresReadableVersionAndSubscription(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "codex")
	for _, tc := range []struct {
		version, auth string
		ok            bool
	}{{"codex-cli 0.160.1", "ChatGPT", true}, {"unreadable synthetic-secret", "ChatGPT", false}, {"codex-cli 0.160.1", "API key", false}} {
		script := "#!/bin/sh\nif [ \"$1\" = --version ]; then printf '%s\\n' '" + tc.version + "'; else printf '%s\\n' '" + tc.auth + "'; fi\n"
		if err := os.WriteFile(cli, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		err := Preflight(context.Background(), cli, "")
		if (err == nil) != tc.ok {
			t.Fatalf("%+v: %v", tc, err)
		}
		if err != nil && strings.Contains(err.Error(), "synthetic-secret") {
			t.Fatal("CLI出力の秘密値を含めました")
		}
	}
}

// 事前確認の失敗は、どのCLIで何が足りないか（終了コード・表示の不足）を示し、CLIの出力は含めない。
func TestPreflightFailureNamesCLIAndReason(t *testing.T) {
	for i, tc := range []struct {
		script, want string
	}{
		{"if [ \"$1\" = --version ]; then echo 'synthetic-secret'; fi", "codexのCLIの版を確認できません（版の表示を読めません）"},
		{"if [ \"$1\" = --version ]; then echo 'synthetic-secret'; exit 4; fi", "codexのCLIの版を確認できません（終了コード 4）"},
		{"if [ \"$1\" = --version ]; then echo 'codex-cli 0.160.1'; else echo 'synthetic-secret'; exit 2; fi", "CodexのChatGPT認証を確認できません（終了コード 2）"},
		{"if [ \"$1\" = --version ]; then echo 'codex-cli 0.160.1'; else echo 'synthetic-secret'; fi", "CodexのChatGPT認証を確認できません（ChatGPTでのログインと表示されません）"},
	} {
		// 同じファイルを書き直して実行すると、並行する子プロセスの起動で text file busy になることがある。
		cli := filepath.Join(t.TempDir(), fmt.Sprintf("codex-%d", i))
		if err := os.WriteFile(cli, []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		err := Preflight(context.Background(), cli, "")
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: %v", tc.want, err)
		}
	}
}

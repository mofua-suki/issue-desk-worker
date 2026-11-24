package hostd

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"issue-desk-worker/internal/process"
)

// 実地確認のgitの失敗は、サブコマンドと終了コードを示す。
func TestLiveGitNamesSubcommandAndExit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("gitがありません")
	}
	r := &Runtime{Run: process.Run}
	_, err := r.liveGit(context.Background(), t.TempDir(), "rev-parse", "--verify", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "git rev-parse") || !strings.Contains(err.Error(), "終了コード") {
		t.Fatalf("失敗の理由がありません: %v", err)
	}
}

// レジストリの資格情報を読めないときは、更新を始めずに失敗を周期の結果に残す。
func TestUpdateWithoutRegistryPasswordRecordsFailure(t *testing.T) {
	r := &Runtime{Config: Configuration{Home: t.TempDir(), ClientDir: t.TempDir()}, Run: func(context.Context, string, []string, process.Options) (process.Result, error) {
		t.Fatal("資格情報が無いのに更新を始めました")
		return process.Result{}, nil
	}}
	if r.update(context.Background(), "v1.2.3") || r.failure == nil || r.failure.Stage != "client" {
		t.Fatalf("失敗を残していません: %+v", r.failure)
	}
	// 先に記録した失敗は上書きしない。
	earlier := &cycleFailure{"docker", "docker_exit", "cycle_failed"}
	r.failure = earlier
	if r.update(context.Background(), "v1.2.3") || r.failure != earlier {
		t.Fatalf("先の失敗を上書きしました: %+v", r.failure)
	}
}

package execution

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 起動時に、異常終了で残った一時領域だけを消す。読めない・書けない階層があっても消す。
func TestSweepStaleTempsRemovesOnlyRunTemps(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("rootでは権限0の階層も読めるため、戻して消す処理を確かめられません")
	}
	root := t.TempDir()
	tmpfs := t.TempDir()
	t.Setenv("ISSUE_DESK_SANDBOX_TMPFS_DIR", tmpfs)
	if err := os.Mkdir(filepath.Join(tmpfs, ".test-9"), 0700); err != nil {
		t.Fatal(err)
	}
	workspaces := filepath.Join(root, "workspaces")
	stale := []string{".test-1", ".sandbox-2", "_isolation_check-3"}
	for _, name := range stale {
		dir := filepath.Join(workspaces, name, "cache", "module")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0400); err != nil {
			t.Fatal(err)
		}
		// Goのモジュールキャッシュのように、読めない・書けない階層を作る。
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	}
	kept := filepath.Join(workspaces, "owner__project", "job-1")
	if err := os.MkdirAll(kept, 0700); err != nil {
		t.Fatal(err)
	}
	if err := sweepStaleTemps(root); err != nil {
		t.Fatalf("消せませんでした: %v", err)
	}
	for _, name := range stale {
		if _, err := os.Lstat(filepath.Join(workspaces, name)); !os.IsNotExist(err) {
			t.Fatalf("%sが残っています: %v", name, err)
		}
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("作業ツリーを消しました: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(tmpfs, ".test-9")); !os.IsNotExist(err) {
		t.Fatalf("tmpfs側の一時領域が残っています: %v", err)
	}
}

// 起動時の復旧で、残った一時領域を消す。
func TestRecoverSweepsStaleTemps(t *testing.T) {
	f := newPipeline(t, "approve")
	stale := filepath.Join(f.Executor.Config.RuntimeDir, "workspaces", ".sandbox-1")
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatalf("起動時に消していません: %v", err)
	}
}

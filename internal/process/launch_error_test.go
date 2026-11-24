package process

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 起動できないコマンドは、実行ファイルの名前（パスなし）と失敗の種類を示し、元の失敗も辿れる。
func TestLaunchErrorNamesExecutableAndKind(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "private", "no-such-tool")
	_, err := Run(context.Background(), missing, nil, Options{})
	var launch *LaunchError
	if !errors.As(err, &launch) || !strings.Contains(err.Error(), "no-such-tool") || !strings.Contains(err.Error(), "見つかりません") || strings.Contains(err.Error(), "private") {
		t.Fatalf("起動の失敗が分かりません: %v", err)
	}
	_, err = Run(context.Background(), "no-such-tool-in-path", nil, Options{})
	if !errors.Is(err, exec.ErrNotFound) || !strings.Contains(err.Error(), "見つかりません") {
		t.Fatalf("PATHに無いことが分かりません: %v", err)
	}
	dir := t.TempDir()
	_, err = Run(context.Background(), dir, nil, Options{})
	if err == nil || !strings.Contains(err.Error(), filepath.Base(dir)) {
		t.Fatalf("実行できないファイルの名前がありません: %v", err)
	}
}

package journal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// 実行状態フォルダーを作れないときは、元の失敗を示す。
func TestOpenKeepsMkdirCause(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(filepath.Join(file, "runtime"))
	if err == nil || !strings.Contains(err.Error(), "作れません") || !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("元の失敗がありません: %v", err)
	}
}

package gitea

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadTokenErrorsAndLimit(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for _, tc := range []struct {
		name, path, want string
	}{
		{"missing", filepath.Join(dir, "missing"), "監視用Giteaトークンを開けません"},
		{"group readable", write("group", "abc", 0640), "監視用Giteaトークンは所有者だけが読める通常ファイルにしてください"},
		{"over limit", write("large", strings.Repeat("a", 1025), 0600), "監視用Giteaトークンを読めません"},
		{"invalid", write("invalid", "a b", 0600), "監視用Giteaトークンの形式が不正です"},
	} {
		if _, err := ReadToken(tc.path); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: %v; want %s", tc.name, err, tc.want)
		}
	}
	// 原因の種類を添え、ファイルの場所は出さない。
	for path, kind := range map[string]string{write("group2", "abc", 0640): "所有者以外も読めます", write("large2", strings.Repeat("a", 1025), 0600): "上限より大きいです", dir: "通常のファイルではありません"} {
		_, err := ReadToken(path)
		if err == nil || !strings.HasSuffix(err.Error(), "（"+kind+"）") || strings.Contains(err.Error(), dir) {
			t.Errorf("%s: 種類がありません: %v", kind, err)
		}
	}
	limit := strings.Repeat("a", 1024)
	if _, err := ReadToken(write("limit", limit+"\n", 0600)); err == nil || !strings.HasPrefix(err.Error(), "監視用Giteaトークンを読めません") {
		t.Fatalf("1025 bytes を受理しました: %v", err)
	}
	if got, err := ReadToken(write("exact", limit, 0600)); err != nil || got != limit {
		t.Fatalf("上限ちょうどを読めません: %v", err)
	}
}

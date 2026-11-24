package config

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
	fill := func(n int) string { return "idk_" + strings.Repeat("a", n-4) }
	for _, tc := range []struct {
		name, path, want string
	}{
		{"missing", filepath.Join(dir, "missing"), "PC別トークンを開けません"},
		{"group readable", write("group", fill(47), 0640), "PC別トークンは所有者だけが読める通常ファイルにしてください"},
		{"invalid", write("invalid", "x", 0600), "PC別トークンの形式が不正です"},
	} {
		if _, err := (Bootstrap{ServerTokenFile: tc.path}).ReadToken(); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("%s: %v; want %s", tc.name, err, tc.want)
		}
	}
	token := fill(47)
	padded := token + strings.Repeat(" ", 256-len(token))
	if got, err := (Bootstrap{ServerTokenFile: write("exact", padded, 0600)}).ReadToken(); err != nil || got != token {
		t.Fatalf("上限ちょうどを読めません: %v", err)
	}
	if _, err := (Bootstrap{ServerTokenFile: write("over", padded+" ", 0600)}).ReadToken(); err == nil || !strings.HasPrefix(err.Error(), "PC別トークンを読めません") {
		t.Fatalf("上限を1つ超えたファイルを受理しました: %v", err)
	}
}

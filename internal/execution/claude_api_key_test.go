package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeAPIKeyFileValidation(t *testing.T) {
	dir := t.TempDir()
	e := &Executor{Config: Configuration{Fleet: Fleet{WorkerTokenFile: filepath.Join(dir, "token")}}}
	path := filepath.Join(dir, "claude-api-key")
	valid := "sk-ant-" + strings.Repeat("a", 20)
	if e.claudeAPIKey() != "" {
		t.Fatal("missing key accepted")
	}
	for _, tc := range []struct {
		name, value string
		mode        os.FileMode
		want        bool
	}{
		{"valid", valid, 0600, true}, {"invalid", "sk-ant-short", 0600, false},
		{"empty", "", 0600, false}, {"permissions", valid, 0644, false},
		{"oversize", "sk-ant-" + strings.Repeat("a", 4090), 0600, false},
		{"newline", valid + "\n", 0600, true}, {"crlf", valid + "\r\n", 0600, true},
		{"multiple newlines", valid + "\n\n", 0600, false}, {"embedded newline", valid + "\nX", 0600, false},
		{"bare cr", valid + "\r", 0600, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			os.Remove(path)
			if err := os.WriteFile(path, []byte(tc.value), tc.mode); err != nil {
				t.Fatal(err)
			}
			// WriteFile applies the process umask; fix the mode so the case is
			// the same under umask 0077 and 022.
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(path); err != nil {
				t.Fatal(err)
			} else if info.Mode().Perm() != tc.mode {
				t.Fatalf("mode = %v, want %v", info.Mode().Perm(), tc.mode)
			}
			if got := e.claudeAPIKey(); (got != "") != tc.want {
				t.Fatal("unexpected key validity")
			} else if tc.want && got != valid {
				t.Fatal("key was not normalized")
			}
		})
	}
	os.Remove(path)
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if e.claudeAPIKey() != "" {
		t.Fatal("symlink accepted")
	}
}

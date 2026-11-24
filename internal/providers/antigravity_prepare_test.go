package providers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareAntigravityKeepsAuthenticationAndEnforcesProtection(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "agy", "settings.json")
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(file, []byte(`{"custom":true,"modelProvider":"credits","toolPermission":"allow"}`), 0600)
	auth := filepath.Join(root, "agy", "auth.json")
	_ = os.WriteFile(auth, []byte("untouched"), 0600)
	p := Policy{Root: filepath.Join(root, "runtime"), TokenFile: filepath.Join(root, "private", "token")}
	if err := PrepareAntigravity(file, p); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(file)
	var cfg map[string]any
	_ = json.Unmarshal(raw, &cfg)
	if cfg["custom"] != true || cfg["modelProvider"] != nil || cfg["toolPermission"] != nil {
		t.Fatal(cfg)
	}
	if err := checkAntigravity(Request{AntigravitySettings: file, Policy: p}); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(auth)
	if string(a) != "untouched" {
		t.Fatal("認証を変更しました")
	}
	stat, _ := os.Stat(file)
	if stat.Mode().Perm() != 0600 {
		t.Fatal(stat.Mode())
	}
}
func TestPrepareAntigravityRejectsWorkspaceCoveringFolder(t *testing.T) {
	root := t.TempDir()
	if err := PrepareAntigravity(filepath.Join(root, "settings.json"), Policy{Root: root, TokenFile: filepath.Join(root, "private", "token")}); err == nil {
		t.Fatal("作業領域を覆う拒否を保存しました")
	}
}

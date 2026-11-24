package testcause

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// 作業ツリーのファイルは通常のファイルだけを読み、symlinkを辿らず、FIFOで止まらない。
func TestTreeFilesAreReadOnlyWhenRegular(t *testing.T) {
	tree := t.TempDir()
	outside := filepath.Join(t.TempDir(), "go.mod")
	if err := os.WriteFile(outside, []byte("module example.invalid/outside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(tree, "inner")
	if err := os.MkdirAll(filepath.Join(tree, "linked"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inner, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, "go.mod"), []byte("module example.invalid/inner\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "linked", "go.mod")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(tree, "package.json"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var modules map[string]string
	var args []string
	var ok bool
	go func() {
		defer close(done)
		modules = goModules(tree)
		args, ok = nodeTestArgs(tree, []string{"npm", "test"})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("FIFOの読取で止まりました")
	}
	if len(modules) != 1 || modules["example.invalid/inner"] != inner {
		t.Fatalf("通常のgo.modだけを読んでいません: %v", modules)
	}
	if ok || args != nil {
		t.Fatalf("FIFOのpackage.jsonを読みました: %v", args)
	}
	if _, err := readTreeFile(tree, "linked/go.mod", 1<<20); err == nil {
		t.Fatal("symlinkを辿りました")
	}
	if err := os.Symlink("inner/go.mod", filepath.Join(tree, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if _, err := readTreeFile(tree, "go.mod", 1<<20); err == nil {
		t.Fatal("ツリー内を指すsymlinkを辿りました")
	}
	if _, err := readTreeFile(tree, "inner/go.mod", 4); err == nil {
		t.Fatal("上限を超えて読みました")
	}
}

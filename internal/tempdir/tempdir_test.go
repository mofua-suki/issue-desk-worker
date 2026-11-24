package tempdir

import (
	"os"
	"path/filepath"
	"testing"
)

// 書けない階層があっても消し、名前の合うものだけを掃除する。
func TestRemoveAndSweep(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("rootでは権限0の階層も読めるため、戻して消す処理を確かめられません")
	}
	root := t.TempDir()
	// 読み取りもできない階層（0000）も置く。
	locked := filepath.Join(root, "checkout-3", "cache")
	if err := os.MkdirAll(filepath.Join(locked, "inner"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	for _, name := range []string{"test-home-1", "checkout-2", "keep"} {
		dir := filepath.Join(root, name, "sdk", "bin")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tool"), []byte("x"), 0500); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	}
	if err := Sweep([]string{root}, []string{"test-home-*", "checkout-*"}); err != nil {
		t.Fatal(err)
	}
	for name, kept := range map[string]bool{"test-home-1": false, "checkout-2": false, "checkout-3": false, "keep": true} {
		if _, err := os.Stat(filepath.Join(root, name)); (err == nil) != kept {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := Remove(filepath.Join(root, "keep")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "keep")); !os.IsNotExist(err) {
		t.Fatalf("書けない階層を消せません: %v", err)
	}
}

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFailureCleansTemporaryFileAndPreservesTarget(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		err := Write(target, ".tmp-", 0600, true, []byte("new"), func(f *os.File) error { return f.Close() })
		if err == nil {
			t.Fatal("expected write failure")
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "old" {
			t.Fatalf("target: %q, %v", got, err)
		}
		matches, err := filepath.Glob(filepath.Join(dir, ".tmp-*"))
		if err != nil || len(matches) != 0 {
			t.Fatalf("temporary files: %v, %v", matches, err)
		}
	})
	t.Run("rename", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
			t.Fatal(err)
		}
		err := write(target, ".tmp-", 0600, true, []byte("new"), nil, func(string, string) error { return errors.New("rename failed") })
		if err == nil {
			t.Fatal("expected rename failure")
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != "old" {
			t.Fatalf("target contents: %q, %v", got, err)
		}
		matches, err := filepath.Glob(filepath.Join(dir, ".tmp-*"))
		if err != nil || len(matches) != 0 {
			t.Fatalf("temporary files: %v, %v", matches, err)
		}
	})
}

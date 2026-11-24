package secretfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRead(t *testing.T) {
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
		name string
		path string
		want string
		err  error
	}{
		{"owner only", write("ok", "abcd", 0600), "abcd", nil},
		{"exact limit", write("limit", strings.Repeat("a", 8), 0600), strings.Repeat("a", 8), nil},
		{"over limit", write("large", strings.Repeat("a", 9), 0600), "", ErrRead},
		{"group readable", write("group", "abcd", 0640), "", ErrUnprotected},
		{"directory", dir, "", ErrUnprotected},
		{"missing", filepath.Join(dir, "missing"), "", ErrOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Read(tc.path, 8)
			if !errors.Is(err, tc.err) || string(got) != tc.want {
				t.Fatalf("Read = %q, %v; want %q, %v", got, err, tc.want, tc.err)
			}
		})
	}
}

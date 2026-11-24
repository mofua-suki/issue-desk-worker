package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppleEntriesValidateBootstrapBeforeNetwork(t *testing.T) {
	for _, mode := range []string{"check", "run"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.json")
			if err := os.WriteFile(path, []byte(`{"serverUrl":"https://unused.invalid","serverTokenFile":"missing","workerId":"apple"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := run([]string{"apple", mode, "--config", path}); err == nil {
				t.Fatal("missing PC credential admitted")
			}
		})
	}
	if err := run([]string{"apple", "unknown"}); err == nil {
		t.Fatal("unknown Apple operation accepted")
	}
}

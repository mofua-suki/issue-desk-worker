package main

import (
	"strings"

	"os"
	"path/filepath"
	"testing"
)

func TestRunRequiresTokenBeforeNetwork(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.local.json")
	if err := os.WriteFile(file, []byte(`{"serverUrl":"https://unreachable.invalid","serverTokenFile":"missing-token","workerId":"test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"run", "--config", file}); err == nil || !strings.Contains(err.Error(), "PC別トークン") {
		t.Fatalf("実行器なしでトークン読取や接続を開始しました: %v", err)
	}
}

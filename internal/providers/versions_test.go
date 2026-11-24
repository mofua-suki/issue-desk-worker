package providers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCLIVersionWireAndMismatch(t *testing.T) {
	cli := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\necho 'codex-cli 0.160.1'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"0.160.1", "0.150.0", ""} {
		rows, err := versionReports(context.Background(), cli, "", map[string]string{"codex": want})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0]["cli"] != "codex" || rows[0]["current"] != "0.160.1" || rows[0]["at"].(int64) <= 0 {
			t.Fatal(rows)
		}
		_, warn := rows[0]["reason"]
		if warn != (want != "0.160.1") {
			t.Fatal(rows)
		}
		if want == "" && rows[0]["expected"] != "不明" {
			t.Fatal("欠測定義を実測版で埋めました")
		}
	}
}

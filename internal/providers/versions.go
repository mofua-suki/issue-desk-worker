package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/process"
	"os"
	"time"
)

func versionReports(ctx context.Context, codex, claude string, expected map[string]string) ([]map[string]any, error) {
	var reports []map[string]any
	for _, cli := range []struct{ name, path string }{{"codex", codex}, {"claude", claude}} {
		if cli.path == "" {
			continue
		}
		out, err := process.Run(ctx, cli.path, []string{"--version"}, process.Options{Timeout: 20 * time.Second})
		current := parseCLIVersion(cli.name, out.Out+"\n"+out.Err)
		if err != nil || out.Code != 0 || current == "" || len(current) > 100 {
			return nil, checkFailure(cli.name+"のCLIの版を確認できません", err, out.Code, "版の表示を読めません")
		}
		want := expected[cli.name]
		reason := ""
		if want == "" {
			want = "不明"
			reason = "イメージの版定義を読めません"
		} else if want != current {
			reason = "CLIの版がイメージの定義と違います"
		}
		row := map[string]any{"cli": cli.name, "current": current, "expected": want, "at": time.Now().UnixMilli()}
		if reason != "" {
			row["reason"] = reason
		}
		reports = append(reports, row)
	}
	return reports, nil
}

// checkFailure names why a CLI check failed without the CLI's output, which
// may carry account details: the kind of a start failure, the exit code, or
// unreadable for an output that did not say what was expected.
func checkFailure(message string, err error, code int, unreadable string) error {
	switch {
	case err != nil:
		return cause.New(message, err)
	case code != 0:
		return fmt.Errorf("%s（終了コード %d）", message, code)
	}
	return fmt.Errorf("%s（%s）", message, unreadable)
}

func PreflightReport(ctx context.Context, codex, claude string) ([]map[string]any, error) {
	expected := map[string]string{}
	if raw, err := os.ReadFile("/app/deploy/versions.json"); err == nil {
		_ = json.Unmarshal(raw, &expected)
	}
	reports, err := versionReports(ctx, codex, claude, expected)
	if err != nil {
		return nil, err
	}
	if err = preflightAuth(ctx, codex, claude); err != nil {
		return nil, err
	}
	return reports, nil
}
func Preflight(ctx context.Context, codex, claude string) error {
	_, err := PreflightReport(ctx, codex, claude)
	return err
}

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	applerunner "issue-desk-worker/internal/apple"
)

// probe は、隔離の検査（check）の結果を表示したうえで、検査に失敗しても、配られた固定のtestCommandを
// 要求と同じ隔離の中で1回流し、出力と終了コードを返す。隔離の規則を直すときにMac上で確かめるための入口で、
// 検証要求の引き取りと受付への結果の送信はしない。
func probe(ctx context.Context, runner *applerunner.Runner, repo, tree string) error {
	check, stop := context.WithTimeout(ctx, 90*time.Second)
	err := runner.Check(check)
	stop()
	if err != nil {
		fmt.Fprintf(os.Stderr, "隔離の検査: 失敗（%v）\n", err)
	} else {
		fmt.Fprintln(os.Stderr, "隔離の検査: 成功")
	}
	out, err := runner.Probe(ctx, repo, tree)
	_, _ = os.Stdout.WriteString(out.Out)
	_, _ = os.Stderr.WriteString(out.Err)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "probe: 終了コード %d\n", out.Code)
	if out.Code != 0 {
		return fmt.Errorf("probeのテストが終了コード %d で終わりました", out.Code)
	}
	return nil
}

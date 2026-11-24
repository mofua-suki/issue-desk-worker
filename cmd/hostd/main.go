package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"issue-desk-worker/internal/hostd"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "update-now" {
		os.Exit(updateNow(os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "live-check" {
		f := flag.NewFlagSet("live-check", flag.ContinueOnError)
		file := f.String("config", "", "既存の実地確認設定")
		if f.Parse(os.Args[2:]) != nil || *file == "" || f.NArg() != 0 {
			os.Exit(2)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := hostd.LiveCheckOnce(ctx, *file); err != nil {
			fmt.Fprintln(os.Stderr, "実地確認を完了できませんでした。安全な保存結果は次回照合します")
			os.Exit(1)
		}
		return
	}

	flags := flag.NewFlagSet("hostd", flag.ContinueOnError)
	config := flags.String("config", "", "hostd設定ファイル")
	check := flags.Bool("check", false, "設定と専用資格情報の検査だけ行う")
	if flags.Parse(os.Args[1:]) != nil {
		os.Exit(2)
	}
	if *config == "" {
		exe, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		*config = filepath.Join(filepath.Dir(exe), "config.json")
	}
	cfg, err := hostd.Load(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	runtime, err := hostd.New(cfg)
	if errors.Is(err, hostd.ErrLocked) {
		// The lock is taken after the configuration and credentials are
		// checked, so a locked --check has nothing left to verify. The user
		// service runs --check before every cycle; another cycle or an
		// update-now holding the lock is a normal wait, not a failure.
		fmt.Println("実行ロック中です")
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer runtime.Close()
	if *check {
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runtime.Cycle(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "hostdの周期処理を完了できませんでした")
		os.Exit(1)
	}
}

func updateNow(args []string) int {
	f := flag.NewFlagSet("update-now", flag.ContinueOnError)
	config := f.String("config", "", "hostd設定ファイル")
	wait := f.Duration("wait", 0, "最大待機時間 (例: 10m)")
	if f.Parse(args) != nil || f.NArg() != 0 || *wait <= 0 {
		fmt.Fprintln(os.Stderr, "正の待機時間を --wait で指定してください")
		return 2
	}
	if *config == "" {
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "設定の場所を確認できませんでした")
			return 1
		}
		*config = filepath.Join(filepath.Dir(exe), "config.json")
	}
	cfg, err := hostd.Load(*config)
	if err != nil {
		fmt.Fprintln(os.Stderr, "hostd設定を確認できませんでした")
		return 1
	}
	runtime, err := hostd.New(cfg)
	if errors.Is(err, hostd.ErrLocked) {
		fmt.Println("実行ロック中です")
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hostdを開始できませんでした")
		return 1
	}
	defer runtime.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	result, err := runtime.ImmediateUpdateWithProgress(ctx, time.Now().Add(*wait), func(r hostd.ImmediateUpdateResult) { printUpdateResult(r) })
	if errors.Is(err, context.Canceled) {
		fmt.Println("待機を中止しました")
		return 2
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "即時反映に失敗しました")
		return 1
	}
	return printUpdateResult(result)
}

func printUpdateResult(r hostd.ImmediateUpdateResult) int {
	messages := map[string]string{
		"success": "更新が完了しました", "same_version": "同一版です", "no_delivery": "配信はありません",
		"pinned": "pin中です", "failed_tag": "failedTagsに含まれます",
		"lazy_wait": "lazyの空きを待っています", "heartbeat_wait": "heartbeatを待っています",
		"new_implementation": "新しい実装を検出しdrainを解除しました", "drain_timeout": "drain期限に到達し解除しました",
		"drain_lost": "drainを確認できず切替を中止しました", "command_pending": "別の指示の完了を待っています",
		"digest_mismatch": "digestが一致せず復旧しました", "ready_failed": "ready期限切れで復旧しました",
		"update_failed": "更新に失敗し復旧しました", "recovered": "中断した更新を復旧しました",
		"report_retried": "保存済み結果を再送しました", "wait_timeout": "待機期限に到達しました", "cancelled": "待機を中止しました",
	}
	if r.Reason == "busy" {
		fmt.Printf("仕事の完了待ち: 実行中 %d、公開中 %d\n", r.Implementing, r.Publishing)
		return 2
	}
	message, ok := messages[r.Reason]
	if !ok {
		fmt.Println("即時反映の結果を確認できませんでした")
		return 1
	}
	if r.Reason == "lazy_wait" || r.Reason == "heartbeat_wait" {
		if r.CountsKnown {
			fmt.Printf("%s（実行中 %d、公開中 %d）\n", message, r.Implementing, r.Publishing)
		} else {
			fmt.Printf("%s（件数未確認）\n", message)
		}
	} else {
		fmt.Println(message)
	}
	if r.Reason == "report_retried" && r.ReportStatus != "success" {
		fmt.Println("保存済みの更新は失敗です")
		return 1
	}
	if (r.Reason == "wait_timeout" || r.Reason == "cancelled") && r.WaitReason != "" {
		printUpdateResult(hostd.ImmediateUpdateResult{Reason: r.WaitReason, Implementing: r.Implementing, Publishing: r.Publishing, CountsKnown: r.CountsKnown})
	}
	switch r.Reason {
	case "success", "same_version", "no_delivery", "pinned", "failed_tag", "report_retried":
		return 0
	case "digest_mismatch", "ready_failed", "update_failed", "recovered":
		return 1
	default:
		return 2
	}
}

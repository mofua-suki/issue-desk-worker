package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"issue-desk-worker/internal/cause"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	applerunner "issue-desk-worker/internal/apple"
	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/intake"
)

func apple(args []string) error {
	usage := "使い方: worker apple run|check --config PATH / worker apple probe --config PATH --repo OWNER/NAME --tree DIR"
	if len(args) == 0 || (args[0] != "run" && args[0] != "check" && args[0] != "probe") {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet("apple", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	file := flags.String("config", "", "既存K2設定")
	repo := flags.String("repo", "", "probeで流すリポジトリ")
	tree := flags.String("tree", "", "probeで流す作業ツリー")
	if err := flags.Parse(args[1:]); err != nil {
		return cause.New("Apple runnerの引数が不正です。"+usage, err)
	}
	// --repo・--tree は probe でだけ両方を指定する。
	if probing := args[0] == "probe"; probing != (*repo != "" && *tree != "") || !probing && (*repo != "" || *tree != "") {
		return errors.New(usage)
	}
	if *file == "" || flags.NArg() != 0 {
		return errors.New("--configを指定してください")
	}
	absolute, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	b, err := config.Load(absolute)
	if err != nil {
		return err
	}
	token, err := b.ReadToken()
	if err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return errors.New("このApple runnerはDarwin専用です")
	}
	api, err := intake.New(b, token, nil)
	if err != nil {
		return err
	}
	runnerWillReport := false
	defer func() {
		if args[0] != "run" || runnerWillReport {
			return
		}
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = api.Heartbeat(c, intake.HostStatus{Ready: false, Capacity: 1})
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	snapshot, err := api.Config(ctx)
	if err != nil {
		return err
	}
	c, err := fleetconfig.DecodeApple(snapshot, b.WorkerID)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	runner, err := applerunner.New(b, c, api, exe)
	if err != nil {
		return err
	}
	if args[0] == "run" {
		runnerWillReport = true
		return runner.Run(ctx)
	}
	if args[0] == "probe" {
		return probe(ctx, runner, *repo, *tree)
	}
	ctx, stop := context.WithTimeout(ctx, 90*time.Second)
	defer stop()
	return runner.Check(ctx)
}

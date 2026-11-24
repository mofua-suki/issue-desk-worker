package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/execution"
	"issue-desk-worker/internal/intake"
)

var revision = "development"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) > 0 && args[0] == "apple" {
		return apple(args[1:])
	}
	if len(args) > 0 && args[0] == "webhook-proxy" {
		return webhookProxy(args[1:])
	}
	if len(args) > 0 && args[0] == "antigravity-prepare" {
		return antigravityPrepare(args[1:])
	}
	if len(args) > 0 && args[0] == "token-place" {
		return tokenPlace(args[1:], os.Stdin)
	}
	if len(args) > 0 && args[0] == "credential" {
		return credential(args[1:], os.Stdin, os.Stdout)
	}
	if len(args) > 0 && args[0] == "isolation-check" {
		return isolationCheck(args[1:])
	}
	if len(args) > 0 && args[0] == "isolation-probe" {
		return isolationProbe(args[1:])
	}
	if len(args) == 1 && args[0] == "version" {
		fmt.Println("issue-desk-worker", revision)
		return nil
	}
	if len(args) < 1 {
		return errors.New("使い方: worker version | worker check --config <設定ファイル> | worker run --config <設定ファイル>")
	}
	mode := args[0]
	f := flag.NewFlagSet(mode, flag.ContinueOnError)
	file := f.String("config", "", "3項目の設定ファイル")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if *file == "" || f.NArg() != 0 {
		return errors.New("--configで設定ファイルを指定してください")
	}
	if mode != "check" && mode != "run" {
		return errors.New("対応していない操作です")
	}
	if runtime.GOOS != "linux" {
		return errors.New("通常WorkerはLinux専用です。Macではworker appleを使用してください")
	}
	b, err := config.Load(*file)
	if err != nil {
		return err
	}
	token, err := b.ReadToken()
	if err != nil {
		return err
	}
	api, err := intake.New(b, token, nil)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if mode == "run" {
		return runController(ctx, api, b.WorkerID)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	snapshot, err := api.Config(ctx)
	if err != nil {
		return err
	}
	configuration, err := execution.DecodeConfig(snapshot, b.WorkerID)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := execution.Check(ctx, configuration, exe); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"workerId": b.WorkerID, "configVersion": snapshot.Version, "configurationVersion": snapshot.ConfigurationVersion, "executionReady": true})
}

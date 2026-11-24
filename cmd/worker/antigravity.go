package main

import (
	"context"
	"errors"
	"flag"
	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/execution"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/providers"
	"time"
)

func antigravityPrepare(args []string) error {
	f := flag.NewFlagSet("antigravity-prepare", flag.ContinueOnError)
	file := f.String("config", "", "3項目の設定ファイル")
	if f.Parse(args) != nil || *file == "" || f.NArg() != 0 {
		return errors.New("antigravity-prepareに--configを指定してください")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	snap, err := api.Config(ctx)
	if err != nil {
		return err
	}
	cfg, err := execution.DecodeConfig(snap, b.WorkerID)
	if err != nil {
		return err
	}
	if cfg.Antigravity == nil {
		return errors.New("Antigravityが管理設定にありません")
	}
	return providers.PrepareAntigravity(cfg.Antigravity.SettingsFile, providers.Policy{Root: cfg.RuntimeDir, TokenFile: cfg.WorkerTokenFile})
}

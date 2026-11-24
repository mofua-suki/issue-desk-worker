package main

import (
	"context"
	"errors"
	"issue-desk-worker/internal/intake"
)

func runController(context.Context, *intake.Client, string) error {
	return errors.New("Darwinではworker appleを使用してください。実装WorkerはDocker DesktopのLinuxで実行してください")
}

package main

import (
	"context"
	"issue-desk-worker/internal/desk"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	status := desk.Command{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}.Run(ctx, os.Args[1:])
	cancel()
	stop()
	os.Exit(status)
}

package main

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/readiness"
	workerruntime "issue-desk-worker/internal/runtime"
	"os"
	"regexp"
	"time"
)

var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func workerRevision() *string {
	value := revision
	if !revisionPattern.MatchString(value) {
		value = os.Getenv("ISSUE_DESK_REVISION")
	}
	if !revisionPattern.MatchString(value) {
		return nil
	}
	return &value
}

// isStartupFailure tells whether the controller's err is reported as a failed
// startup: not when the worker is stopping or restarts for a changed
// configuration, which has already reported ready=false and is not broken.
func isStartupFailure(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil && !errors.Is(err, workerruntime.ErrConfigChanged)
}

// A failed startup clears any previous ready=true without requesting or completing a job.
func reportStartupFailure(ctx context.Context, api *intake.Client, report *readiness.Report, stage string, capacity int) {
	report.Record(stage, false)
	if capacity < 1 || capacity > 8 {
		capacity = 1
	}
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	rev := workerRevision()
	if err := api.Heartbeat(reportCtx, intake.HostStatus{Revision: rev, ImageRevision: rev, Ready: false, Capacity: capacity, Checks: report.Value()}); err != nil {
		fmt.Fprintln(os.Stderr, "起動検査の不合格を受付へ送信できませんでした")
	}
}

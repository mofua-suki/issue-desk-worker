package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"issue-desk-worker/internal/execution"
	"issue-desk-worker/internal/failurelog"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/hostload"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/readiness"
	workerruntime "issue-desk-worker/internal/runtime"
	"issue-desk-worker/internal/webhook"
	"os"
	"time"
)

func runController(ctx context.Context, api *intake.Client, host string) (startupErr error) {
	report := &readiness.Report{At: time.Now()}
	stage := "server"
	capacity := 1
	defer func() {
		if isStartupFailure(ctx, startupErr) {
			reportStartupFailure(ctx, api, report, stage, capacity)
		}
	}()

	snapshot, err := api.Config(ctx)
	if err != nil {
		return err
	}
	report.Record("server", true)
	stage = "config"
	configuration, err := execution.DecodeConfig(snapshot, host)
	if err != nil {
		return err
	}
	report.SettingsVersion = snapshot.SettingsVersion
	report.Record("config", true)
	capacity = configuration.Capacity()
	stage = "runtime"
	controller, err := execution.New(ctx, configuration, api)
	if err != nil {
		var failure *execution.StartupError
		if errors.As(err, &failure) {
			stage = failure.Name
		}
		return err
	}
	defer controller.Close()
	if err := controller.InitializeSettings(snapshot); err != nil {
		return err
	}
	for _, name := range []string{"giteaToken", "giteaScopes", "volume", "cli"} {
		report.Record(name, true)
	}
	stage = "isolation"
	if err := controller.VerifyIsolation(ctx); err != nil {
		return err
	}
	report.Record("isolation", true)
	stage = "recovery"
	if err := controller.Recover(ctx); err != nil {
		return err
	}
	report.Record("recovery", true)
	stage = "webhook"
	if configuration.Webhook == nil {
		return errors.New("Webhookの配信設定がありません")
	}
	verifier := configuration.Webhook.TokenSHA256
	if configuration.Webhook.Managed {
		verifier = configuration.WebhookVerifier
		if verifier == "" {
			verifier = configuration.Webhook.TokenSHA256
		}
	}
	if configuration.Webhook.TokenFile != "" && !configuration.Webhook.Managed && verifier == "" {
		secret, err := gitea.ReadToken(configuration.Webhook.TokenFile)
		if err != nil {
			return err
		}
		hash := sha256.Sum256([]byte("Bearer " + secret))
		verifier = hex.EncodeToString(hash[:])
	}
	receiver, err := webhook.New(verifier)
	if err != nil {
		return err
	}
	if err := receiver.Listen(*configuration.Webhook); err != nil {
		return err
	}
	report.Record("webhook", true)
	defer receiver.Close()
	controller.StartCommands(ctx)
	// Deferred after Close, so the polling ends before the journal closes.
	defer controller.StopCommands()
	nextVerifierCheck := time.Time{}
	cleaner := &execution.WorkspaceCleaner{Executor: controller}
	maintenance := func(ctx context.Context, eligible []string) error {
		if configuration.Webhook.Managed && time.Now().After(nextVerifierCheck) {
			nextVerifierCheck = time.Now().Add(30 * time.Second)
			snapshot, err := api.Config(ctx)
			if err != nil {
				return err
			}
			fresh, err := execution.DecodeConfig(snapshot, host)
			if err != nil {
				return err
			}
			candidate := managedVerifier(verifier, fresh.WebhookVerifier, snapshot.WebhookVerifierState)
			if err := receiver.SetVerifier(candidate); err != nil {
				return err
			}
			verifier = candidate
		}
		return maintainWorkspaces(ctx, eligible, controller.RefreshHostEnvironment, func(context.Context, []string) error { return nil }, cleaner.Cleanup)
	}
	runningRevision := workerRevision()
	stage = "runtime"
	meter := hostload.New(configuration.RuntimeDir, hostload.Source{})
	loadCtx, stopLoad := context.WithCancel(ctx)
	defer stopLoad()
	go meter.Run(loadCtx)
	heartbeatStatus := func() intake.HostStatus {
		status := controller.HeartbeatStatus()
		status.Load = meter.Measure(time.Now())
		return status
	}
	maintenanceFailures := &failurelog.Log{Label: "依頼確認を完了できませんでした"}
	runner := workerruntime.Runner{CanAcquire: controller.CanAcquire, Status: heartbeatStatus, StartupChecks: report.Value(), PollInterval: time.Duration(configuration.Assignments.PollSeconds) * time.Second, API: api, Executor: controller, Revision: runningRevision, ImageRevision: runningRevision, PollMaintenanceFor: maintenance, MaintenanceWithFullTest: controller.ProcessRequestsWithFullTestFor, BusyMaintenanceFor: controller.ProcessBusyRequestsFor, OnMaintenanceError: maintenanceFailures.Observe, OnHeartbeatError: func(err error) { fmt.Fprintln(os.Stderr, "heartbeatを送れませんでした:", err) }, Wake: receiver.Wake(), ConfigurationVersion: configuration.Version, Capacity: configuration.Capacity(), RepoLimits: configuration.RepoLimits(), Capabilities: controller.Capabilities()}
	return runner.Run(ctx)
}

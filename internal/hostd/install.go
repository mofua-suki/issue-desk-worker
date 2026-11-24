package hostd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func commandTag(c HostCommand, env environment, host string, now time.Time) (string, string) {
	if c.Kind != "install" {
		return "", "unsupported"
	}
	if !now.Before(c.ExpiresAt) {
		return "", "expired"
	}
	tag := c.Tag
	if strings.HasSuffix(tag, "-python") && !strings.HasSuffix(tag, "-client-python") {
		tag = strings.TrimSuffix(tag, "-python") + "-client-python"
	}
	python := strings.HasSuffix(tag, "-client-python")
	if !tagPattern.MatchString(tag) || python != (env.Flavor == "-python") || c.Host != "" && c.Host != host {
		return "", "no_target"
	}
	return tag, ""
}

func (r *Runtime) acceptCommand(ctx context.Context, env environment, snapshot imageSnapshot, waiting *UpdateState) (bool, error) {
	if snapshot.Command == nil {
		return false, nil
	}
	c := snapshot.Command
	if c.ID == "" {
		return true, errors.New("指示のIDがありません")
	}
	receipts, err := r.receipts()
	if err != nil {
		return true, err
	}
	if receipt := receipts[c.ID]; receipt != nil {
		if !receipt.Pending {
			return false, nil
		}
		return true, r.sendCommandResult(ctx, receipts, receipt)
	}
	tag, reason := commandTag(*c, env, r.Config.Host, r.Now())
	if reason != "" {
		r.recordInstallOutcome(*c, reason)
		receipt := &commandReceipt{Command: *c, Result: &CommandResult{ID: c.ID, Reason: reason, At: r.Now().UTC()}, Pending: true}
		receipts[c.ID] = receipt
		if err := r.save("command-results.json", receipts); err != nil {
			return true, err
		}
		return true, r.sendCommandResult(ctx, receipts, receipt)
	}
	s := &UpdateState{Command: c, CommandHost: r.Config.Host, Phase: "command-pulling", Target: Target{Tag: tag, Flavor: env.Flavor}, OldTag: env.Tag}
	if waiting != nil {
		s.DrainSet, s.DrainAttempt, s.Until = waiting.DrainSet, waiting.DrainAttempt, waiting.Until
	}
	// The command ID and original contents reach durable storage before pulling,
	// draining, stopping or replacing any existing waiting update.
	if err := r.save("state.json", s); err != nil {
		return true, err
	}
	return true, r.runInstall(ctx, s)
}

func (r *Runtime) runInstall(ctx context.Context, s *UpdateState) error {
	if s.CommandHost != r.Config.Host {
		return errors.New("保存した指示の宛先PCが現在の設定と異なります")
	}
	switch s.Phase {
	case "command-reporting":
		return r.finishCommand(ctx, s)
	case "command-stopping", "updating", "recovering":
		// A stop or switch may already have taken effect. Never repeat the install.
		if s.Status == "" {
			s.Status = "switch_failed"
		}
		return r.recoverInstall(context.WithoutCancel(ctx), s)
	case "command-pulling", "command-draining":
	default:
		return errors.New("installの保存段階が不正です")
	}
	env, err := r.environment()
	if err != nil {
		return err
	}
	if _, reason := commandTag(*s.Command, env, r.Config.Host, r.Now()); reason != "" {
		return r.commandOutcome(ctx, s, reason)
	}
	if s.Phase == "command-pulling" {
		if !r.update(ctx, s.Target.Tag, "--pull-only") {
			return r.commandOutcome(ctx, s, "tag_missing")
		}
		target, err := r.completeTarget(ctx, s.Target)
		if err != nil || !validInstallTarget(target, env.Flavor) {
			return r.commandOutcome(ctx, s, "verify_failed")
		}
		snapshot, err := r.snapshot(ctx, env)
		if err != nil {
			return err
		}
		if snapshot.Desired != nil && snapshot.Desired.Tag == target.Tag {
			if !validTarget(*snapshot.Desired, env.Flavor) || target.Digest != snapshot.Desired.Digest || target.Registry != snapshot.Desired.Registry {
				return r.commandOutcome(ctx, s, "verify_failed")
			}
			target.Revision = snapshot.Desired.Revision
		}
		s.Target = target
		s.Phase, s.DrainAttempt = "command-draining", true
		s.Started = r.Now()
		s.Deadline = r.Now().Add(time.Duration(r.Config.WaitMinutes) * time.Minute)
		if err := r.save("state.json", s); err != nil {
			return err
		}
		var result struct {
			Drain *Drain `json:"drain"`
		}
		until := s.Deadline.Add(40*time.Minute + time.Duration(r.Config.ReadySeconds)*time.Second)
		if err := r.api(ctx, "PUT", "drain", map[string]any{"until": until.UTC().Format(time.RFC3339Nano)}, &result); err != nil {
			return r.commandOutcome(context.WithoutCancel(ctx), s, "failed")
		}
		if result.Drain == nil || result.Drain.By != "hostd:"+r.Config.Host || !result.Drain.Until.After(r.Now()) || result.Drain.At.After(r.Now()) || result.Drain.At.Before(s.Started) {
			return r.commandOutcome(ctx, s, "failed")
		}
		s.Started, s.Until = result.Drain.At, result.Drain.Until
		s.DrainSet, s.DrainAttempt = true, false
		return r.save("state.json", s)
	}
	snapshot, err := r.snapshot(ctx, env)
	if err != nil {
		return err
	}
	// Recheck the original ID, contents and expiry after waiting for heartbeat.
	if snapshot.Command == nil || !sameHostCommand(*snapshot.Command, *s.Command) {
		return r.commandOutcome(ctx, s, "no_target")
	}
	if !r.Now().Before(s.Command.ExpiresAt) {
		return r.commandOutcome(ctx, s, "expired")
	}
	mark := snapshot.Heartbeat.mark()
	if !s.DrainSet || mark == nil || mark.By != "hostd:"+r.Config.Host || mark.At.Before(s.Started) || !mark.Until.After(r.Now()) || !r.Now().Before(s.Until) {
		return r.commandOutcome(ctx, s, "failed")
	}
	if !fresh(snapshot.Heartbeat, s.Started, r.Now()) || !numeric(snapshot.Heartbeat) {
		if !r.Now().Before(s.Deadline) {
			return r.commandOutcome(ctx, s, "failed")
		}
		return nil
	}
	// No lazy wait or idle decision here: a fresh drain mark is sufficient even
	// with publishing jobs. Container termination completes/cancels their work.
	s.Phase = "command-stopping"
	if err := r.save("state.json", s); err != nil {
		return err
	}
	if err := r.stopWorker(ctx); err != nil {
		s.Status = "failed"
		return r.recoverInstall(context.WithoutCancel(ctx), s)
	}
	if !r.Now().Before(s.Command.ExpiresAt) {
		s.Status = "expired"
		return r.recoverInstall(context.WithoutCancel(ctx), s)
	}
	if !r.Now().Before(s.Until) {
		s.Status = "failed"
		return r.recoverInstall(context.WithoutCancel(ctx), s)
	}
	s.Phase, s.UpdateStarted = "updating", r.Now()
	if err := r.save("state.json", s); err != nil {
		return err
	}
	if !r.update(ctx, s.Target.Tag) {
		s.Status = "switch_failed"
		return r.recoverInstall(context.WithoutCancel(ctx), s)
	}
	if !r.verifyDigest(ctx, s.Target) || !r.verifyRunningDigest(ctx, s.Target) || !r.waitReady(ctx, s) {
		s.Status = "verify_failed"
		return r.recoverInstall(context.WithoutCancel(ctx), s)
	}
	return r.commandOutcome(context.WithoutCancel(ctx), s, "done")
}

func validInstallTarget(t Target, flavor string) bool {
	if !tagPattern.MatchString(t.Tag) {
		return false
	}
	// completeTarget cannot recover a full commit for a non-desired release.
	copy := t
	copy.Revision = t.Tag[:12] + strings.Repeat("0", 28)
	return validTarget(copy, flavor)
}

func (r *Runtime) recoverInstall(ctx context.Context, s *UpdateState) error {
	s.Phase = "recovering"
	if err := r.save("state.json", s); err != nil {
		return err
	}
	env, err := r.environment()
	_, pending := os.Stat(filepath.Join(r.Config.ClientDir, ".linux-update.pending"))
	intact := errors.Is(pending, os.ErrNotExist) && err == nil && env.Tag == s.OldTag
	if intact {
		// The switch script may have failed during authentication/pull, before
		// saving an environment. The stopped old container must still be started.
		if _, err := r.workerCompose(ctx, "up", "-d", "--no-deps", "--pull", "never", "issue-desk-client"); err != nil {
			return err
		}
	} else if !r.update(ctx, "--restore-previous") {
		return errors.New("Workerを直前の版へ戻せません")
	}
	return r.commandOutcome(ctx, s, s.Status)
}

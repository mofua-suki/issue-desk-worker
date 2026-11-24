package hostd

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

var registryPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(?::[0-9]{1,5})?$`)

type Target struct {
	Tag      string `json:"tag"`
	Flavor   string `json:"flavor"`
	Digest   string `json:"digest"`
	Revision string `json:"revision"`
	Registry string `json:"registry"`
	Kind     string `json:"kind"`
}
type Drain struct {
	By    string    `json:"by"`
	At    time.Time `json:"at"`
	Until time.Time `json:"until"`
}
type Heartbeat struct {
	At           time.Time `json:"heartbeat_at"`
	Implementing *int      `json:"implementing"`
	Publishing   *int      `json:"publishing"`
	Revision     string    `json:"revision"`
	Ready        bool      `json:"ready"`
	Drain        struct {
		Hostd *Drain    `json:"hostd"`
		By    string    `json:"by"`
		At    time.Time `json:"at"`
		Until time.Time `json:"until"`
	} `json:"drain"`
}

func (h *Heartbeat) mark() *Drain {
	if h == nil {
		return nil
	}
	if h.Drain.Hostd != nil {
		return h.Drain.Hostd
	}
	if h.Drain.By != "" {
		return &Drain{h.Drain.By, h.Drain.At, h.Drain.Until}
	}
	return nil
}

type imageSnapshot struct {
	Desired    *Target      `json:"desired"`
	Heartbeat  *Heartbeat   `json:"heartbeat"`
	Pin        any          `json:"pin"`
	FailedTags []string     `json:"failedTags"`
	Command    *HostCommand `json:"command"`
}
type UpdateState struct {
	Command       *HostCommand   `json:"command,omitempty"`
	CommandHost   string         `json:"commandHost,omitempty"`
	CommandResult *CommandResult `json:"commandResult,omitempty"`
	Phase         string         `json:"phase"`
	Target        Target         `json:"target"`
	Started       time.Time      `json:"started"`
	Deadline      time.Time      `json:"deadline"`
	Until         time.Time      `json:"until"`
	Previous      *Heartbeat     `json:"previous"`
	DrainSet      bool           `json:"drainSet"`
	DrainAttempt  bool           `json:"drainAttempt"`
	LazySince     time.Time      `json:"lazySince"`
	OldTag        string         `json:"oldTag"`
	UpdateStarted time.Time      `json:"updateStarted"`
	ReadyDeadline time.Time      `json:"readyDeadline,omitempty"`
	Status        string         `json:"status"`
}

func validTarget(t Target, flavor string) bool {
	suffix := ""
	if flavor == "-python" {
		suffix = "-client-python"
	}
	return shaPattern.MatchString(t.Revision) && t.Tag == t.Revision[:12]+suffix && t.Flavor == flavor && imageDigestPattern.MatchString(t.Digest) && registryPattern.MatchString(t.Registry)
}
func numeric(h *Heartbeat) bool {
	return h != nil && h.Implementing != nil && h.Publishing != nil && *h.Implementing >= 0 && *h.Publishing >= 0
}
func fresh(h *Heartbeat, since, now time.Time) bool {
	return h != nil && h.At.After(since) && h.At.After(now.Add(-15*time.Minute)) && !h.At.After(now)
}
func drainDecision(h, previous *Heartbeat, since, now, deadline time.Time) string {
	valid := numeric(h) && fresh(h, since, now) && (previous == nil || h.At.After(previous.At))
	if valid {
		if numeric(previous) && *h.Implementing > *previous.Implementing {
			return "new_implementation"
		}
		if *h.Implementing == 0 && *h.Publishing == 0 {
			return "idle"
		}
	}
	if !now.Before(deadline) {
		return "drain_timeout"
	}
	if valid {
		return "progress"
	}
	return "wait"
}
func (r *Runtime) snapshot(ctx context.Context, e environment) (imageSnapshot, error) {
	var out imageSnapshot
	q := url.Values{"flavor": {e.Flavor}, "currentRevision": {e.Tag}}
	err := r.api(ctx, "GET", "desired-image?"+q.Encode(), nil, &out)

	return out, err
}
func (r *Runtime) finishUpdate(ctx context.Context, s *UpdateState) error {
	if s.Phase == "reporting" {
		if !tagPattern.MatchString(s.Target.Tag) || !imageDigestPattern.MatchString(s.Target.Digest) {
			return errors.New("復旧報告のimage情報が不正です")
		}
		if err := r.api(ctx, "POST", "image-result", map[string]any{"tag": s.Target.Tag, "flavor": s.Target.Flavor, "digest": s.Target.Digest, "status": s.Status}, nil); err != nil {
			return err
		}
		s.Phase = "releasing"
		if err := r.save("state.json", s); err != nil {
			return err
		}
	}
	if err := r.api(ctx, "DELETE", "drain", nil, nil); err != nil {
		return err
	}
	return r.save("state.json", nil)
}
func (r *Runtime) recoverUpdate(ctx context.Context, s *UpdateState) error {
	s.Phase = "recovering"
	if err := r.save("state.json", s); err != nil {
		return err
	}
	env, err := r.environment()
	_, pending := os.Stat(filepath.Join(r.Config.ClientDir, ".linux-update.pending"))
	intact := pending != nil && errors.Is(pending, os.ErrNotExist) && err == nil && s.OldTag != "" && env.Tag == s.OldTag
	if !intact && !r.update(ctx, "--restore-previous") {
		return errors.New("Workerを直前の版へ戻せません")
	}
	s.Phase = "reporting"
	s.Status = "failed"
	if err := r.save("state.json", s); err != nil {
		return err
	}
	return r.finishUpdate(ctx, s)
}
func (r *Runtime) updateCycle(ctx context.Context) error {
	return r.updateStep(ctx, nil, time.Time{})
}

func (r *Runtime) updateStep(ctx context.Context, result *ImmediateUpdateResult, waitUntil time.Time) error {
	u := &updateRun{r: r, ctx: ctx, result: result, waitUntil: waitUntil}
	for _, step := range []func() (bool, error){u.resumeSaved, u.readDelivery, u.skipDelivery, u.prepareWait, u.startDrain, u.waitDrain} {
		if done, err := step(); done {
			return err
		}
	}
	return u.swap()
}

// updateRun is the state of one update step: the saved state, this PC's
// environment and the intake's image snapshot. result is nil for the
// periodic cycle and set for an immediate update.
type updateRun struct {
	r         *Runtime
	ctx       context.Context
	result    *ImmediateUpdateResult
	waitUntil time.Time
	state     *UpdateState
	env       environment
	snapshot  imageSnapshot
	now       time.Time
}

// set records the reason of an immediate update.
func (u *updateRun) set(reason string) {
	if u.result != nil {
		u.result.Reason = reason
	}
}

// resumeSaved continues a saved command or phase. A pending update marker
// without an update in progress means an interrupted swap, which is recovered.
func (u *updateRun) resumeSaved() (bool, error) {
	r, ctx := u.r, u.ctx
	if err := r.load("state.json", &u.state); err != nil {
		return true, err
	}
	state := u.state
	if state != nil && state.Command != nil {
		if u.result != nil {
			u.set("command_pending")
			return true, nil
		}
		return true, r.runInstall(ctx, state)
	}
	if _, err := os.Stat(filepath.Join(r.Config.ClientDir, ".linux-update.pending")); err == nil && (state == nil || state.Phase != "updating" && state.Phase != "recovering" && state.Phase != "readying") {
		if state == nil {
			env, readErr := r.environment()
			if readErr != nil {
				return true, readErr
			}
			target, completeErr := r.completeTarget(ctx, Target{Tag: env.Tag, Flavor: env.Flavor})
			if completeErr != nil {
				return true, completeErr
			}
			state = &UpdateState{Phase: "recovering", Target: target}
			u.state = state
			if err := r.save("state.json", state); err != nil {
				return true, err
			}
		}
		state.Phase = "recovering"
		if err := r.save("state.json", state); err != nil {
			return true, err
		}
	}
	if state != nil {
		switch state.Phase {
		case "readying":
			return true, r.finishReady(ctx, state, u.result, u.waitUntil)
		case "updating", "recovering":
			u.set("recovered")
			return true, r.recoverUpdate(ctx, state)
		case "reporting", "releasing":
			u.set("report_retried")
			if u.result != nil {
				u.result.ReportStatus = state.Status
			}
			return true, r.finishUpdate(ctx, state)
		case "waiting":
		default:
			return true, errors.New("Worker更新の保存段階が不正です")
		}
	}
	return false, nil
}

// readDelivery reads this PC's image and the intake's snapshot, accepts a
// periodic command, and records whether a drain attempt took effect.
func (u *updateRun) readDelivery() (bool, error) {
	r, ctx, result := u.r, u.ctx, u.result
	if err := r.retryCommandResults(ctx); err != nil {
		return true, err
	}
	var err error
	u.env, err = r.environment()
	if err != nil {
		return true, err
	}
	u.snapshot, err = r.snapshot(ctx, u.env)
	if err != nil {
		return true, err
	}
	snapshot, state := u.snapshot, u.state
	if result != nil && numeric(snapshot.Heartbeat) {
		result.Implementing = *snapshot.Heartbeat.Implementing
		result.Publishing = *snapshot.Heartbeat.Publishing
		result.CountsKnown = true
	}
	if result == nil {
		if handled, err := r.acceptCommand(ctx, u.env, snapshot, state); handled || err != nil {
			return true, err
		}
	}
	u.now = r.Now()
	if state != nil && state.DrainAttempt {
		mark := snapshot.Heartbeat.mark()
		state.DrainSet = mark != nil && mark.By == "hostd:"+r.Config.Host
		if state.DrainSet {
			if mark.At.After(state.Started) {
				state.Started = mark.At
			}
			state.Until = mark.Until
		}
		state.DrainAttempt = false
		if err := r.save("state.json", state); err != nil {
			return true, err
		}
	}
	return false, nil
}

// skipDelivery ends a step without a new image to install, releasing this
// PC's drain and the saved state.
func (u *updateRun) skipDelivery() (bool, error) {
	r, snapshot, state, env := u.r, u.snapshot, u.state, u.env
	desired := snapshot.Desired
	skip := desired == nil || snapshot.Pin != nil || desired != nil && desired.Tag == env.Tag
	if desired != nil {
		for _, tag := range snapshot.FailedTags {
			if tag == desired.Tag {
				skip = true
			}
		}
	}
	if !skip {
		return false, nil
	}
	switch {
	case desired == nil:
		u.set("no_delivery")
	case snapshot.Pin != nil:
		u.set("pinned")
	case desired.Tag == env.Tag:
		u.set("same_version")
	default:
		u.set("failed_tag")
	}
	if state != nil {
		mark := snapshot.Heartbeat.mark()
		if state.DrainSet && (mark == nil || mark.By == "hostd:"+r.Config.Host) {
			if err := r.api(u.ctx, "DELETE", "drain", nil, nil); err != nil {
				return true, err
			}
		}
		return true, r.save("state.json", nil)
	}
	return true, nil
}

// prepareWait checks the delivered image and starts, or retargets, the wait
// for it.
func (u *updateRun) prepareWait() (bool, error) {
	r, snapshot, now := u.r, u.snapshot, u.now
	desired := snapshot.Desired
	if !validTarget(*desired, u.env.Flavor) {
		return true, errors.New("配信されたWorker imageが不正です")
	}
	if u.state != nil && (u.state.Target.Tag != desired.Tag || u.state.Target.Digest != desired.Digest) {
		if !u.state.DrainSet {
			u.state = nil
		} else {
			u.state.Target = *desired
			if err := r.save("state.json", u.state); err != nil {
				return true, err
			}
		}
	}
	if u.state == nil {
		u.state = &UpdateState{Phase: "waiting", Target: *desired, Started: now, Deadline: now.Add(time.Duration(r.Config.WaitMinutes) * time.Minute)}
		if numeric(snapshot.Heartbeat) && !snapshot.Heartbeat.At.After(now) {
			u.state.Previous = snapshot.Heartbeat
		}
		if err := r.save("state.json", u.state); err != nil {
			return true, err
		}
	}
	return false, nil
}

// startDrain stops new acquisitions on this PC, after the lazy wait of a lazy
// delivery. The drain is confirmed by a later heartbeat.
func (u *updateRun) startDrain() (bool, error) {
	r, ctx, snapshot, state, now := u.r, u.ctx, u.snapshot, u.state, u.now
	if state.DrainSet {
		return false, nil
	}
	if snapshot.Desired.Kind == "lazy" {
		if state.LazySince.IsZero() {
			state.LazySince = now
			if err := r.save("state.json", state); err != nil {
				return true, err
			}
		}
		if now.Before(state.LazySince.Add(12*time.Hour)) && (!numeric(snapshot.Heartbeat) || !fresh(snapshot.Heartbeat, state.LazySince, now) || *snapshot.Heartbeat.Implementing != 0 || *snapshot.Heartbeat.Publishing != 0) {
			u.set("lazy_wait")
			return true, nil
		}
	}
	if !state.LazySince.IsZero() {
		state.Started = now
		state.Deadline = now.Add(time.Duration(r.Config.WaitMinutes) * time.Minute)
		if numeric(snapshot.Heartbeat) && fresh(snapshot.Heartbeat, time.Time{}, now) {
			state.Previous = snapshot.Heartbeat
		}
	}
	state.DrainAttempt = true
	if err := r.save("state.json", state); err != nil {
		return true, err
	}
	var result struct {
		Drain *Drain `json:"drain"`
	}
	if err := r.api(ctx, "PUT", "drain", map[string]any{"until": state.Deadline.Add(40*time.Minute + time.Duration(r.Config.ReadySeconds)*time.Second).UTC().Format(time.RFC3339Nano)}, &result); err != nil {
		return true, err
	}
	if result.Drain == nil || !result.Drain.Until.After(r.Now()) {
		return true, errors.New("新しい引取の停止を確認できません")
	}
	if result.Drain.At.After(state.Started) {
		state.Started = result.Drain.At
	}
	state.Until = result.Drain.Until
	state.DrainSet = true
	state.DrainAttempt = false
	u.set("heartbeat_wait")
	return true, r.save("state.json", state)
}

// waitDrain waits for this PC to go idle under its drain. A new
// implementation, a timeout or a lost drain gives up the update.
func (u *updateRun) waitDrain() (bool, error) {
	r, ctx, snapshot, state, now, result := u.r, u.ctx, u.snapshot, u.state, u.now, u.result
	since := state.Started
	decision := drainDecision(snapshot.Heartbeat, state.Previous, since, now, state.Deadline)
	if decision == "new_implementation" || decision == "drain_timeout" || !now.Before(state.Until) {
		if decision == "new_implementation" {
			u.set("new_implementation")
		} else {
			u.set("drain_timeout")
		}
		if err := r.api(ctx, "DELETE", "drain", nil, nil); err != nil {
			return true, err
		}
		return true, r.save("state.json", nil)
	}
	if decision != "idle" {
		if decision == "progress" {
			u.set("busy")
			if result != nil {
				result.Implementing = *snapshot.Heartbeat.Implementing
				result.Publishing = *snapshot.Heartbeat.Publishing
			}
		} else {
			u.set("heartbeat_wait")
		}
		if decision == "progress" {
			state.Previous = snapshot.Heartbeat
			return true, r.save("state.json", state)
		}
		return true, nil
	}
	mark := snapshot.Heartbeat.mark()
	if mark == nil || mark.By != "hostd:"+r.Config.Host || !mark.Until.After(now) {
		u.set("drain_lost")
		if mark == nil || mark.By == "hostd:"+r.Config.Host {
			if err := r.api(ctx, "DELETE", "drain", nil, nil); err != nil {
				return true, err
			}
		}
		return true, r.save("state.json", nil)
	}
	return false, nil
}

// swap installs the target image and waits for it to become ready, or
// restores the previous image.
func (u *updateRun) swap() error {
	r, ctx, state, result := u.r, u.ctx, u.state, u.result
	state.Phase = "updating"
	state.OldTag = u.env.Tag
	state.UpdateStarted = r.Now()
	if err := r.save("state.json", state); err != nil {
		return err
	}
	updated := r.update(ctx, state.Target.Tag)
	verified := updated && r.verifyDigest(ctx, state.Target)
	if updated && !verified && r.failure == nil {
		u.set("digest_mismatch")
		// 登録されたregistry・digestと、このPCの起動中のimageが違う。待たずに戻すので、ready_timeoutと区別する。
		r.failure = &cycleFailure{"swap-start", "other", "digest_mismatch"}
	}
	if verified {
		state.Phase = "readying"
		if err := r.save("state.json", state); err != nil {
			return err
		}
		return r.finishReady(ctx, state, result, u.waitUntil)
	}
	if result != nil && result.Reason == "" {
		if !updated {
			u.set("update_failed")
		} else {
			u.set("digest_mismatch")
		}
	}
	if r.failure == nil {
		r.failure = &cycleFailure{"swap-start", "timeout", "ready_timeout"}
	}
	return r.recoverUpdate(context.WithoutCancel(ctx), state)
}

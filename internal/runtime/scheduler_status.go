package runtime

import (
	"context"
	"errors"
	"time"

	"issue-desk-worker/internal/failurelog"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/readiness"
)

// settingsExecutor is an executor that applies the shared settings snapshot
// itself and reports its version.
type settingsExecutor interface {
	ApplySettingsSnapshot(intake.ConfigSnapshot) error
	SettingsVersion() string
}

type reportedVersion struct {
	value   any
	present bool
}

// loadStartupSettings reads the configuration, checks the startup report and
// applies the first settings snapshot.
func (s *scheduler) loadStartupSettings() error {
	r := s.r
	snapshot, err := r.API.Config(s.ctx)
	if err != nil {
		return err
	}
	s.configuredVersion = r.ConfigurationVersion
	if s.configuredVersion == "" {
		s.configuredVersion = snapshot.ConfigurationVersion
	}
	s.checks = r.StartupChecks
	if s.checks == nil {
		report := readiness.Report{At: r.Now(), SettingsVersion: snapshot.SettingsVersion}
		report.Record("runtime", true)
		s.checks = report.Value()
	}
	if s.checks["ok"] != true {
		return errors.New("起動検査が不合格のため仕事を取得しません")
	}
	s.settings, s.sharedSettings = r.Executor.(settingsExecutor)
	s.sharedSettings = s.sharedSettings && s.settings.SettingsVersion() != ""
	initialVersion, hasInitialVersion := s.checks["settingsVersion"]
	s.settingsVersion.Store(reportedVersion{value: initialVersion, present: hasInitialVersion})
	if s.sharedSettings && (s.configuredVersion == "" || snapshot.ConfigurationVersion == s.configuredVersion) {
		if err := s.applySettings(snapshot); err != nil && r.OnMaintenanceError != nil {
			r.OnMaintenanceError(err)
		}
	}
	return nil
}

// applySettings applies a settings snapshot, ordered with the heartbeats.
func (s *scheduler) applySettings(next intake.ConfigSnapshot) error {
	s.heartbeatMu.Lock()
	defer s.heartbeatMu.Unlock()
	if s.sharedSettings {
		return s.settings.ApplySettingsSnapshot(next)
	}
	s.settingsVersion.Store(reportedVersion{value: next.SettingsVersion, present: true})
	return nil
}

// checkConfig reads the configuration once a minute. A changed configuration
// version restarts the worker once its jobs end; otherwise the settings
// snapshot is queued for the publisher.
func (s *scheduler) checkConfig() {
	if s.r.Now().Before(s.nextConfigCheck) {
		return
	}
	s.nextConfigCheck = s.r.Now().Add(time.Minute)
	next, err := s.r.API.Config(s.taskCtx)
	if err != nil {
		if s.r.OnMaintenanceError != nil {
			s.r.OnMaintenanceError(err)
		}
	} else if s.configuredVersion != "" && next.ConfigurationVersion != s.configuredVersion {
		// Acquisition stops until the restart, so report not ready once.
		if !s.changedConfig {
			s.acquisitionStopped.Store(true)
			s.requestHeartbeat()
		}
		s.changedConfig = true
	} else {
		select {
		case s.settingsUpdates <- next:
		default:
			select {
			case <-s.settingsUpdates:
			default:
			}
			s.settingsUpdates <- next
		}
	}
}

// startSettingsPublisher keeps publication ordered after any in-flight
// heartbeat without blocking the scheduling loop. Only the newest pending
// snapshot needs publishing.
func (s *scheduler) startSettingsPublisher() {
	s.settingsUpdates = make(chan intake.ConfigSnapshot, 1)
	s.background.Add(1)
	go func() {
		defer s.background.Done()
		for {
			select {
			case <-s.taskCtx.Done():
				return
			case next := <-s.settingsUpdates:
				if err := s.applySettings(next); err != nil && s.r.OnMaintenanceError != nil {
					s.r.OnMaintenanceError(err)
				}
			}
		}
	}()
}

// startHeartbeats sends a heartbeat every HeartbeatInterval and on request.
func (s *scheduler) startHeartbeats() {
	s.background.Add(1)
	s.heartbeatRequested = make(chan struct{}, 1)
	go func() {
		defer s.background.Done()
		timer := time.NewTicker(s.r.HeartbeatInterval)
		defer timer.Stop()
		failures := newHeartbeatFailures(&s.r)
		for {
			select {
			case <-s.taskCtx.Done():
				return
			case <-timer.C:
				failures.observe(s.taskCtx, &s.r, s.heartbeat(s.taskCtx))
			case <-s.heartbeatRequested:
				failures.observe(s.taskCtx, &s.r, s.heartbeat(s.taskCtx))
			}
		}
	}()
}

// heartbeatFailures reports background heartbeat failures through
// OnHeartbeatError, thinned by failurelog on the Runner's clock.
type heartbeatFailures struct{ thinner failurelog.Thinner }

func newHeartbeatFailures(r *Runner) *heartbeatFailures {
	return &heartbeatFailures{thinner: failurelog.Thinner{Now: r.Now}}
}

func (f *heartbeatFailures) observe(ctx context.Context, r *Runner, err error) {
	if err == nil {
		f.thinner.Pass(nil)
		return
	}
	if ctx.Err() != nil || r.OnHeartbeatError == nil {
		return
	}
	if ok, reported := f.thinner.Pass(err); ok {
		r.OnHeartbeatError(reported)
	}
}

// requestHeartbeat asks for a heartbeat without waiting for it.
func (s *scheduler) requestHeartbeat() {
	select {
	case s.heartbeatRequested <- struct{}{}:
	default:
	}
}

func (s *scheduler) heartbeat(ctx context.Context) error {
	s.heartbeatMu.Lock()
	defer s.heartbeatMu.Unlock()
	return s.r.API.Heartbeat(ctx, s.status())
}

// status is the host status reported in a heartbeat.
func (s *scheduler) status() intake.HostStatus {
	r := s.r
	n := int(s.running.Load())
	maintaining := s.maintenance.Load()
	checkCopy := make(map[string]any, len(s.checks))
	for key, value := range s.checks {
		checkCopy[key] = value
	}
	if s.sharedSettings {
		checkCopy["settingsVersion"] = s.settings.SettingsVersion()
	} else if version := s.settingsVersion.Load().(reportedVersion); version.present {
		checkCopy["settingsVersion"] = version.value
	}
	out := intake.HostStatus{Revision: r.Revision, ImageRevision: r.ImageRevision, Ready: !s.acquisitionStopped.Load(), Busy: n > 0, Implementing: n, Capacity: r.Capacity, Capabilities: r.Capabilities, Checks: checkCopy}
	if r.Status != nil {
		extra := r.Status()
		out.Load = extra.Load
		out.CLIVersions = extra.CLIVersions
		out.IntakeHold = extra.IntakeHold
		out.Publishing = extra.Publishing
		out.Implementing = max(0, n-extra.Publishing)
		out.Busy = out.Busy || extra.Publishing > 0
		if extra.Capabilities != nil {
			out.Capabilities = extra.Capabilities
		}
	}
	if maintaining {
		out.Publishing++
		out.Busy = true
	}
	return out
}

package execution

import (
	"context"
	"errors"
	"sync"

	"issue-desk-worker/internal/intake"
)

// settingsSnapshot publishes a decoded configuration and its version together.
// Readers retain an immutable configuration even if a later refresh is applied.
type settingsSnapshot struct {
	mu      sync.RWMutex
	config  Configuration
	version string
}

func (e *Executor) InitializeSettings(snapshot intake.ConfigSnapshot) error {
	fresh, err := DecodeConfig(snapshot, e.Config.Host)
	if err != nil {
		return err
	}
	if snapshot.ConfigurationVersion != e.Config.Version || snapshot.SettingsVersion == "" {
		return errors.New("設定の版が一致しません")
	}
	e.settingsSnapshot = &settingsSnapshot{config: fresh, version: snapshot.SettingsVersion}
	return nil
}

// ApplySettingsSnapshot is called by the scheduler after it checks for a
// configuration change. A failed decode never replaces the working snapshot.
func (e *Executor) ApplySettingsSnapshot(snapshot intake.ConfigSnapshot) error {
	if snapshot.ConfigurationVersion != e.Config.Version || snapshot.SettingsVersion == "" {
		return errors.New("設定の版が一致しません")
	}
	fresh, err := DecodeConfig(snapshot, e.Config.Host)
	if err != nil {
		return err
	}
	s := e.settingsSnapshot
	if s == nil {
		return errors.New("運用設定が初期化されていません")
	}
	s.mu.Lock()
	s.config, s.version = fresh, snapshot.SettingsVersion
	s.mu.Unlock()
	return nil
}

func (e *Executor) SettingsVersion() string {
	s := e.settingsSnapshot
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

func (e *Executor) operationalConfig(ctx context.Context) (Configuration, error) {
	if s := e.settingsSnapshot; s != nil {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.config, nil
	}
	// Executors constructed outside the worker controller retain the existing
	// on-demand behaviour.
	snapshot, err := e.API.Config(ctx)
	if err != nil {
		return Configuration{}, err
	}
	return DecodeConfig(snapshot, e.Config.Host)
}

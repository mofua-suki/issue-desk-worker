package execution

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"os"
	"sync"
	"time"
)

type Executor struct {
	settingsSnapshot *settingsSnapshot
	holdsMu          sync.Mutex
	holds            hostHolds
	distributedKey   distributedKeyCache
	cliVersions      []map[string]any
	observationsMu   sync.Mutex
	requestsNext     time.Time
	busyRequestsNext time.Time
	secret           string
	notificationsMu  sync.Mutex
	controls         jobControls
	commands         commandLoop

	Config         Configuration
	API            *intake.Client
	Gitea          *gitea.Client
	Store          *journal.Store
	stageStore     interface{ Save(*journal.Record) error }
	Executable     string
	ready          bool
	Call           func(context.Context, providers.Request) (providers.Answer, error)
	transientWait  func(context.Context, time.Duration) error
	mergeRetryWait func(context.Context, time.Duration) error
	mergeReadWait  func(context.Context, time.Duration) error
	mergeReadNow   func() time.Time
	browserTools   func() (string, string)
}

func New(ctx context.Context, config Configuration, api *intake.Client) (*Executor, error) {
	if config.TestOnly() {
		return nil, errors.New("test-onlyは実装Workerを起動できません")
	}
	token, err := gitea.ReadToken(config.WorkerTokenFile)
	if err != nil {
		return nil, startupError("giteaToken", err)
	}
	g, err := gitea.New(config.GiteaURL, token)
	if err != nil {
		return nil, startupError("giteaToken", err)
	}
	if warning := gitea.PlainHTTPWarning(config.GiteaURL); warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	if err := verifyWorkerAccount(ctx, config, g); err != nil {
		return nil, startupError("giteaToken", err)
	}
	if err := verifyWorkerScopes(ctx, config, g); err != nil {
		return nil, startupError("giteaScopes", err)
	}
	if err := verifyRuntimeVolume(config.RuntimeDir); err != nil {
		return nil, startupError("volume", err)
	}
	store, err := journal.Open(config.RuntimeDir)
	if err != nil {
		return nil, startupError("volume", err)
	}
	exe, err := os.Executable()
	if err != nil {
		store.Close()
		return nil, startupError("volume", err)
	}
	e := &Executor{secret: token, Config: config, API: api, Gitea: g, Store: store, Executable: exe, Call: providers.Call}
	if err := e.loadHostHolds(); err != nil {
		store.Close()
		return nil, startupError("volume", err)
	}
	reports, err := providers.PreflightReport(ctx, config.Codex, config.Claude)
	if err != nil {
		store.Close()
		return nil, startupError("cli", err)
	}
	e.cliVersions = reports
	e.ready = true
	return e, nil
}

func (e *Executor) Close() error { return e.Store.Close() }

func (e *Executor) Ready() bool { return e.ready }

func (e *Executor) Capabilities() map[string]any {
	repos := []string{}
	for _, r := range e.Config.Repositories {
		repos = append(repos, r.Name)
	}
	names := []string{"codex"}
	if e.Config.Claude != "" {
		names = append(names, "claude")
	}
	if e.Config.Antigravity != nil {
		names = append(names, "antigravity")
	}
	return map[string]any{"executionPlanVersion": 1, "environment": e.executionCapabilities(), "platform": "linux", "providers": names, "repositories": repos, "workerTokenKind": "worker"}
}

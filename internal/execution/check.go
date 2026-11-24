package execution

import (
	"context"
	"errors"
	"fmt"
	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/providers"
	"os"
)

// Check validates configured credentials and the real sandbox without acquiring jobs or the journal lock.
func Check(ctx context.Context, config Configuration, executable string) error {
	if config.TestOnly() {
		return errors.New("test-onlyは実装Workerを起動できません")
	}
	token, err := gitea.ReadToken(config.WorkerTokenFile)
	if err != nil {
		return err
	}
	g, err := gitea.New(config.GiteaURL, token)
	if err != nil {
		return err
	}
	if warning := gitea.PlainHTTPWarning(config.GiteaURL); warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	if err := verifyWorkerAccount(ctx, config, g); err != nil {
		return err
	}
	if err := verifyWorkerScopes(ctx, config, g); err != nil {
		return err
	}
	if err := verifyRuntimeVolume(config.RuntimeDir); err != nil {
		return err
	}
	if err := providers.Preflight(ctx, config.Codex, config.Claude); err != nil {
		return err
	}
	e := &Executor{Config: config, Executable: executable}
	return e.VerifyIsolation(ctx)
}

func verifyWorkerAccount(ctx context.Context, c Configuration, g *gitea.Client) error {
	return fleetconfig.VerifyWorkerAccount(ctx, c, g)
}

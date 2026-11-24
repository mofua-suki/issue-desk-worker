package apple

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/process"
)

// Probe runs the distributed fixed testCommand of repo once against an
// existing working tree, inside the same sandbox as a native test request
// (runSandbox). It claims no request and reports nothing to the intake; it is
// the entry point (worker apple probe) for checking isolation rules on a Mac.
func (a *Runner) Probe(ctx context.Context, repo, tree string) (process.Result, error) {
	repos, err := a.Config.NativeRepositories()
	if err != nil {
		return process.Result{}, err
	}
	var policy *fleetconfig.Repository
	for i := range repos {
		if repos[i].Name == repo {
			policy = &repos[i]
		}
	}
	if policy == nil {
		return process.Result{}, fmt.Errorf("%s はこのMacのネイティブテストの対象ではありません", repo)
	}
	abs, err := filepath.Abs(tree)
	if err != nil {
		return process.Result{}, err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return process.Result{}, fmt.Errorf("作業ツリーがありません: %s", tree)
	}
	timeout := policy.TestTimeoutSeconds
	if timeout <= 0 {
		timeout = a.Config.TestTimeoutSeconds
	}
	if err = os.MkdirAll(a.Root, 0700); err != nil {
		return process.Result{}, err
	}
	return a.runSandbox(ctx, abs, policy.TestCommand, time.Duration(timeout)*time.Second, *policy)
}

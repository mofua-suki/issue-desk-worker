package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/process"
	"issue-desk-worker/internal/providers"
)

var npmPackagePattern = regexp.MustCompile(`^(?:@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)
var npmVersionPattern = regexp.MustCompile(`^[A-Za-z0-9^~*>=<|.+_-]+$`)

const chromiumProbeWarning = "Chromiumを起動できない実行環境のためブラウザ試験を省略します"

func (e *Executor) sandbox(ctx context.Context, r *journal.Record, command []string, timeout int) (process.Result, error) {
	if len(command) == 0 {
		return process.Result{}, errors.New("実行するコマンドがありません")
	}
	command = e.resolveTestCommand(command)
	if command[0] == "" {
		return process.Result{}, errors.New("実行コマンドの道具が設定されていません")
	}
	temp, onTmpfs, err := makeSandboxTemp(e.Config.RuntimeDir, r.Tree, e.Config.WorkerTokenFile, true)
	if err != nil {
		if saveErr := e.recordEnvironmentHold(err); saveErr != nil {
			return process.Result{}, errors.Join(err, saveErr)
		}
		return process.Result{}, err
	}
	out, runErr := e.runSandboxTemp(ctx, r, command, timeout, temp)
	logSandboxTempRemoval(removeSandboxTemp(temp))
	if onTmpfs && ctx.Err() == nil && sandboxOutOfSpace(out, runErr) {
		temp, _, err = makeSandboxTemp(e.Config.RuntimeDir, r.Tree, e.Config.WorkerTokenFile, false)
		if err != nil {
			if saveErr := e.recordEnvironmentHold(err); saveErr != nil {
				return process.Result{}, errors.Join(err, saveErr)
			}
			return process.Result{}, err
		}
		firstElapsed := out.Elapsed
		out, runErr = e.runSandboxTemp(ctx, r, command, timeout, temp)
		out.Elapsed += firstElapsed
		logSandboxTempRemoval(removeSandboxTemp(temp))
	}
	// Keep the refusal visible to the caller even when the hold could not be saved.
	if err := e.recordEnvironmentHold(runErr); err != nil {
		return out, errors.Join(runErr, err)
	}
	return out, runErr
}

func (e *Executor) runSandboxTemp(ctx context.Context, r *journal.Record, command []string, timeout int, temp string) (process.Result, error) {
	caddy, chromium := "/opt/agent-tools/bin/caddy", "/usr/lib/chromium/chromium-headless-shell"
	if e.browserTools != nil {
		caddy, chromium = e.browserTools()
	}
	return e.runSandboxTempWithBrowserTools(ctx, r, command, timeout, temp, caddy, chromium)
}

func (e *Executor) runSandboxTempWithBrowserTools(ctx context.Context, r *journal.Record, command []string, timeout int, temp, caddy, chromium string) (process.Result, error) {
	// Go telemetry can spawn a detached writer that recreates an already removed
	// sandbox. Disable it only in this disposable configuration directory.
	telemetry := filepath.Join(temp, "config", "go", "telemetry")
	if err := os.MkdirAll(telemetry, 0700); err != nil {
		return process.Result{}, err
	}
	if err := os.WriteFile(filepath.Join(telemetry, "mode"), []byte("off\n"), 0600); err != nil {
		return process.Result{}, err
	}
	policy := providers.Policy{Root: e.Config.RuntimeDir, Tree: r.Tree, TokenFile: e.Config.WorkerTokenFile, Temp: temp, Tools: []string{e.Config.Codex, e.Config.Git, e.Config.Node, e.Config.PNPM, e.Config.Python}}
	if tool, err := exec.LookPath(command[0]); err == nil {
		policy.Tools = append(policy.Tools, tool)
	}
	args := []string{"sandbox", "-P", "gitea-worker"}
	args = append(args, policy.Args()...)
	args = append(args, "-C", r.Tree, "--")
	args = append(args, command...)
	env := testSandboxEnv(temp)
	err := addIntakeBrowserTools(env, r, caddy, chromium, func(chromium string) error {
		probeArgs := []string{"sandbox", "-P", "gitea-worker"}
		probeArgs = append(probeArgs, policy.Args()...)
		probeArgs = append(probeArgs, "-C", r.Tree, "--", chromium, "--headless", "--no-sandbox", "--disable-gpu", "--disable-background-networking", "--disable-dev-shm-usage", "--user-data-dir="+filepath.Join(temp, "chromium-probe"), "--dump-dom", "about:blank")
		out, err := process.Run(ctx, e.Config.Codex, probeArgs, process.Options{Dir: r.Tree, Timeout: time.Duration(timeout) * time.Second, Env: env, KillGroupOnExit: true})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || out.Code != 0 || out.Interrupted {
			return errors.New(chromiumProbeWarning)
		}
		return nil
	})
	if ctx.Err() != nil {
		return process.Result{}, ctx.Err()
	}
	// Servers and daemons a test starts in the background end with the test,
	// as on the Apple runner; they must not pile up in the long-lived worker.
	out, runErr := process.Run(ctx, e.Config.Codex, args, process.Options{Dir: r.Tree, Timeout: time.Duration(timeout) * time.Second, Env: env, GracefulCancel: true, KillGroupOnExit: true})
	if err != nil {
		out.Err = err.Error() + "\n" + out.Err
	}
	if runErr == nil && outsideTestPermission(out, r.Tree) {
		runErr = testEnvironmentError{}
	}
	return out, runErr
}

// testSandboxEnv is the environment of the fixed-test sandbox. CI=true keeps tools such as pnpm from stopping at an
// interactive prompt (for example, confirming the removal of node_modules) when there is no terminal.
func testSandboxEnv(temp string) map[string]string {
	// ToolchainEnv prepares the JVM agent here; it reads proxy values only after
	// Codex starts the sandbox, just as it does for writable AI sessions.
	env := providers.ToolchainEnv(temp)
	for key, value := range map[string]string{"CI": "true", "TEMP": temp, "TMPDIR": temp, "TZ": "UTC", "XDG_CONFIG_HOME": filepath.Join(temp, "config"), "XDG_CACHE_HOME": filepath.Join(temp, "cache"), "XDG_DATA_HOME": filepath.Join(temp, "data"), "XDG_STATE_HOME": filepath.Join(temp, "state"), "GOCACHE": filepath.Join(temp, "go-build"), "GOMODCACHE": filepath.Join(temp, "go-mod"), "GOPATH": filepath.Join(temp, "go"), "GOPROXY": "https://proxy.golang.org", "GOSUMDB": "sum.golang.org", "GOTOOLCHAIN": "local"} {
		env[key] = value
	}
	return env
}

func addIntakeBrowserTools(env map[string]string, r *journal.Record, caddy, chromium string, probe func(string) error) error {
	if r.Repo != "mofua-suki/issue-desk-intake" {
		return nil
	}
	for _, path := range []string{caddy, chromium} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			return nil
		}
	}
	if err := probe(chromium); err != nil {
		return err
	}
	env["ISSUE_DESK_TEST_CADDY"] = caddy
	env["ISSUE_DESK_TEST_CHROMIUM"] = chromium
	return nil
}

func (e *Executor) fixedTest(ctx context.Context, repo Repository, r *journal.Record, prepare ...func() error) (TestResult, error) {
	result, err := e.measuredTest(ctx, repo, r, "fixed-test", prepare...)
	result.AppliedEdits = r.AppliedEdits
	if err == nil && r.Timing != nil && r.Timing.Generation == r.Generation && r.Timing.Epoch == r.Epoch {
		entry := journal.FixedTestTiming{Passed: result.Passed}
		if len(r.Timing.TestSteps) > 0 {
			step := r.Timing.TestSteps[len(r.Timing.TestSteps)-1]
			if step.Step == "testCommand" && step.ElapsedMillis != nil {
				ms := result.ElapsedMS
				entry.ElapsedMS = &ms
			}
		}
		r.Timing.FixedTestCount++
		if len(r.Timing.FixedTests) < journal.MaxFixedTestTimings {
			r.Timing.FixedTests = append(r.Timing.FixedTests, entry)
		}
		if saveErr := e.saveTiming(r); saveErr != nil {
			return result, saveErr
		}
	}
	return result, err
}

type finalApprovalTestKey struct{}

func (e *Executor) runMeasuredTest(ctx context.Context, repo Repository, r *journal.Record, source string, prepare ...func() error) (TestResult, error) {
	steps, timingErr := e.beginTimingSteps(r, []string{"prettier", "goFmt", "testCommand"})
	if timingErr != nil {
		return TestResult{}, timingErr
	}
	if err := dependencyPolicy(r.Tree); err != nil {
		return TestResult{Command: repo.TestCommand, Detail: err.Error()}, nil
	}
	timeout := repo.TestTimeoutSeconds
	if timeout <= 0 {
		timeout = e.Config.TestTimeoutSeconds
	}
	preserveHead, _ := ctx.Value(finalApprovalTestKey{}).(bool)
	if !preserveHead {
		if err := e.applyRequests(ctx, r); err != nil {
			return TestResult{Command: repo.TestCommand, Detail: err.Error(), accessDenied: accessDeniedError(err)}, nil
		}
		// Formatting and dependency installation happen inside the same constrained test environment.
		if data, err := readSafeFile(r.Tree, "package.json", 8<<20); err == nil && e.Config.PNPM != "" {
			var manifest struct {
				Scripts         map[string]string `json:"scripts"`
				DevDependencies map[string]string `json:"devDependencies"`
			}
			_ = json.Unmarshal(data, &manifest)
			if manifest.DevDependencies["prettier"] != "" {
				out, err := e.timedSandbox(ctx, r, steps, 0, []string{e.Config.PNPM, "exec", "prettier", "--write", "."}, timeout)
				if err != nil {
					return TestResult{}, err
				}
				if out.Code != 0 {
					detail := e.commandFailureDetail("整形が失敗しました", out)
					return TestResult{Command: repo.TestCommand, Detail: detail, evidenceReason: true}, nil
				}
			}
		}
		if _, err := os.Stat(filepath.Join(r.Tree, "go.mod")); err == nil {
			out, err := e.timedSandbox(ctx, r, steps, 1, []string{"go", "fmt", "./..."}, timeout)
			if err != nil {
				return TestResult{}, err
			}
			if out.Code != 0 {
				return TestResult{Command: repo.TestCommand, Detail: e.commandFailureDetail("Goの整形が失敗しました", out), evidenceReason: true}, nil
			}
		}
	}
	var before string
	if preserveHead {
		if r.Provider == "claude" {
			_, err := os.Lstat(filepath.Join(r.Tree, ".claude-staged"))
			if err == nil {
				return TestResult{Command: repo.TestCommand, Detail: "種類=Claudeステージング 対象=.claude-staged 作成時刻=不明 workerId=不明 状態=未処理", evidenceMissing: true}, nil
			}
			if !os.IsNotExist(err) {
				return TestResult{Command: repo.TestCommand, Detail: "種類=Claudeステージング 対象=.claude-staged 作成時刻=不明 workerId=不明 状態=不確か", evidenceMissing: true}, nil
			}
		}
		if detail := pendingEditDetail(r); detail != "" {
			return TestResult{Command: repo.TestCommand, Detail: e.redact(detail), evidenceMissing: true, evidenceReason: true}, nil
		}
		var err error
		before, err = e.testHead(ctx, r)
		if ctx.Err() != nil {
			return TestResult{}, ctx.Err()
		}
		if err != nil || before != r.Head {
			return TestResult{Command: repo.TestCommand, Detail: "開始時のheadまたは作業ツリーを照合できません", evidenceMissing: true}, nil
		}
	} else if len(prepare) > 0 {
		if err := prepare[0](); err != nil {
			var fenceErr testFenceError
			if errors.As(err, &fenceErr) {
				return TestResult{}, fenceErr.error
			}
			if ctx.Err() != nil {
				return TestResult{}, ctx.Err()
			}
			return TestResult{Command: repo.TestCommand, Detail: "固定テスト対象を確定できません: " + err.Error(), evidenceMissing: true}, nil
		}
		var err error
		before, err = e.testHead(ctx, r)
		if ctx.Err() != nil {
			return TestResult{}, ctx.Err()
		}
		if err != nil {
			return TestResult{Command: repo.TestCommand, Detail: "固定テスト対象を照合できません: " + err.Error(), evidenceMissing: true}, nil
		}
		if before != r.Head {
			return TestResult{Command: repo.TestCommand, Detail: "確定したheadと実測対象が一致しません", evidenceMissing: true}, nil
		}
		if r.AppliedEdits != nil && r.AppliedEdits.Head == "" {
			r.AppliedEdits.Head = before
			if err := e.Store.Save(r); err != nil {
				return TestResult{}, err
			}
		}
	}
	started := time.Now()
	out, err := e.timedSandbox(ctx, r, steps, 2, repo.TestCommand, timeout)
	finished := time.Now()
	observation, environment := e.testObservationContext(ctx, r, repo.TestCommand, out, err, started, finished, source)
	stderr := out.Err
	warning := ""
	if strings.HasPrefix(stderr, chromiumProbeWarning+"\n") {
		warning = chromiumProbeWarning + "\n"
		stderr = strings.TrimPrefix(stderr, warning)
	}
	detail := out.Out + "\n" + stderr
	if err != nil {
		detail += "\n" + err.Error()
	}
	result := TestResult{AppliedEdits: r.AppliedEdits, Observation: observation, Passed: err == nil && out.Code == 0, Command: safeTestCommand(e, repo.TestCommand), Detail: warning + failureExcerpt(detail, 4000-len([]rune(warning))), ElapsedMS: time.Since(started).Milliseconds()}
	result.Diagnostic = e.testDiagnostic(ctx, r, source, repo.TestCommand, observation, environment, out, err, started, finished, timeout)
	result.accessDenied = accessDeniedError(err)
	if preserveHead || len(prepare) > 0 {
		after, headErr := e.testHead(ctx, r)
		if headErr != nil || after != before {
			rejectTestEvidence(&result, "固定テスト実行中に対象を照合できないか変更されました")
		} else if result.Passed {
			if reason := testEvidenceReason(result, before, r.Base); reason != "" {
				rejectTestEvidence(&result, reason)
			}
		}
	}
	if result.Passed && observation["complete"] != true && completedTestFiles(r.Tree, e.resolveTestCommand(repo.TestCommand)) == 0 {
		result.Detail = "ラッパー経由などのため完了数を確認できません\n" + result.Detail
	}
	result.countableFailure = !result.evidenceMissing && err == nil && out.Code != 0 && out.Code != 126 && out.Code != 127 && !out.Interrupted && observation["environmentFailure"] == nil
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

func (e *Executor) commandFailureDetail(reason string, out process.Result) string {
	prefix := fmt.Sprintf("%s (終了コード %d)\n", reason, out.Code)
	return prefix + failureExcerpt(e.redact(out.Out+"\n"+out.Err), 4000-len([]rune(prefix)))
}

func (e *Executor) resolveTestCommand(command []string) []string {
	resolved := append([]string{}, command...)
	for i, value := range resolved {
		switch value {
		case "{node}":
			resolved[i] = e.Config.Node
		case "{python}":
			resolved[i] = e.Config.Python
		}
	}
	return resolved
}

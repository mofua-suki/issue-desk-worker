package execution

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/process"
	"issue-desk-worker/internal/providers"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const registryFailureLog = "[WARN] GET https://registry.npmjs.org/typescript error (ECONNRESET). Will retry in 10 seconds. 2 retries left.\n" +
	"✗ Lockfile failed supply-chain policy check (1 entry in 4m 38.5s)\n" +
	"[ERR_PNPM_META_FETCH_FAIL] GET https://registry.npmjs.org/typescript: fetch failed\n\n" +
	"[WARN] Issue while reading \"/home/node/.npmrc\". EACCES: permission denied, open '/home/node/.npmrc'\n"

func TestOutsideTestPermissionBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, text        string
		code              int
		interrupted, want bool
	}{
		{"外部", "EACCES: permission denied, lstat '/outside/cache'", 1, false, true},
		{"相対の祖先", "EPERM: denied, open '../cache'", 1, false, true},
		{"内部", "EACCES: denied, open '/work/tree/cache'", 1, false, false},
		{"合格0でも実測成功", "EACCES: denied, open '/outside'", 0, false, false},
		{"TAP合格混在", "ok 1 - test\nEACCES: denied, open '/outside'", 1, false, false},
		{"spec合格混在", "ℹ pass 1\nEPERM: denied, open '/outside'", 1, false, false},
		{"pytest合格混在", "1 passed\nEPERM: denied, open '/outside'", 1, false, false},
		{"取消", "EPERM: denied, open '/outside'", 1, true, false},
		{"Goの合格行とEACCES", "ok  \tissue-desk-server/internal/api\t0.412s\nEACCES: denied, open '/outside'", 1, false, false},
		{"Goのcached合格行とEACCES", "ok  \tissue-desk-server/internal/api\t(cached)\nEACCES: denied, open '/outside'", 1, false, false},
		{"Goのパッケージ行は失敗のみなら合格でない", "FAIL\tissue-desk-server/internal/api\t0.412s\nEACCES: denied, open '/outside'", 1, false, true},
		{"pnpmのnpmrc警告と通信失敗", registryFailureLog, 1, false, false},
		{"角括弧なしのpnpm警告", " WARN  Issue while reading \"/home/node/.npmrc\". EACCES: permission denied, open '/home/node/.npmrc'\n", 1, false, false},
		{"警告の後の本物の権限エラー", "[WARN] Issue while reading \"/home/node/.npmrc\". EACCES: permission denied, open '/home/node/.npmrc'\nError: EACCES: permission denied, mkdir '/outside/cache'", 1, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := outsideTestPermission(process.Result{Code: tc.code, Err: tc.text, Interrupted: tc.interrupted}, "/work/tree")
			if got != tc.want {
				t.Fatal(got, tc)
			}
		})
	}
}
func TestRegistryFailureWithNpmrcWarningIsOrdinaryFailure(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", `printf '%s\n' "$0" >&2; exit 1`, registryFailureLog}
	calls := 0
	original := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, r providers.Request) (providers.Answer, error) {
		calls++
		return original(ctx, r)
	}
	out, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || out.Status != "failed" || calls < 2 || !f.Executor.CanAcquire() {
		t.Fatalf("通信失敗を環境問題にしました: %v %v calls=%d ready=%v", out, err, calls, f.Executor.CanAcquire())
	}
	r, _ := f.Executor.Store.Load(1)
	var result TestResult
	if json.Unmarshal(r.Test, &result) != nil || result.Unavailable {
		t.Fatal(string(r.Test))
	}
}
func TestOutsidePermissionPersistsAndStopsRepairOnDirtyTree(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "echo \"EACCES: permission denied, lstat '/outside/cache'\" >&2; exit 1"}
	calls := 0
	original := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, r providers.Request) (providers.Answer, error) {
		calls++
		return original(ctx, r)
	}
	out, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || out.Status != "failed" || calls != 1 || f.Executor.CanAcquire() || !environmentStop(t, out) {
		t.Fatalf("環境問題を再実装: %v %v calls=%d ready=%v", out, err, calls, f.Executor.CanAcquire())
	}
	r, _ := f.Executor.Store.Load(1)
	var result TestResult
	if json.Unmarshal(r.Test, &result) != nil || !result.Unavailable || result.countableFailure {
		t.Fatal(string(r.Test))
	}
	if _, err := os.Stat(filepath.Join(r.Tree, "feature.txt")); err != nil {
		t.Fatal("dirty作業を失いました", err)
	}
	again := &Executor{Config: f.Executor.Config}
	if err := again.loadHostHolds(); err != nil || again.CanAcquire() {
		t.Fatal("再起動で保留を失いました", err)
	}
}

// environmentStop reports whether the state sent to the intake marks an environment problem (autoRetry=false).
func environmentStop(t *testing.T, state intake.State) bool {
	t.Helper()
	var view struct {
		Environment map[string]any `json:"environment"`
	}
	if len(state.View) > 0 {
		if err := json.Unmarshal(state.View, &view); err != nil {
			t.Fatal(err)
		}
	}
	return view.Environment != nil
}

// A hold raised outside the fixed test (here by an AI call) must not turn an ordinary failure into an environment stop.
func TestEnvironmentHoldWithoutDeniedTestRepairsAndKeepsHold(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	completionAPI(t, f)
	base := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := base(ctx, req)
		if !req.Review {
			calls++
			if calls == 1 {
				if removeErr := os.Remove(filepath.Join(req.Policy.Tree, "feature.txt")); removeErr != nil {
					t.Fatal(removeErr)
				}
				if holdErr := f.Executor.recordEnvironmentHold(&os.PathError{Op: "open", Path: "/private/cache", Err: os.ErrPermission}); holdErr != nil {
					return answer, errors.Join(err, holdErr)
				}
			}
		}
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || calls != 2 {
		t.Fatalf("保留だけで修復と合格を止めました: state=%+v err=%v calls=%d", state, err, calls)
	}
	if environmentStop(t, state) {
		t.Fatalf("環境問題の印が付きました: %s", state.View)
	}
	if f.Executor.CanAcquire() {
		t.Fatal("合格でホストの環境保留を解除しました")
	}
	again := &Executor{Config: f.Executor.Config}
	if err := again.loadHostHolds(); err != nil || again.CanAcquire() {
		t.Fatal("再起動で保留を失いました", err)
	}
}

func TestEnvironmentHoldWithoutDeniedTestStopsAsOrdinaryFailure(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "echo '--- FAIL: TestBroken'; exit 1"}
	base := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		if !req.Review {
			calls++
		}
		answer, err := base(ctx, req)
		if holdErr := f.Executor.recordEnvironmentHold(&os.PathError{Op: "open", Path: "/private/cache", Err: os.ErrPermission}); holdErr != nil {
			return answer, errors.Join(err, holdErr)
		}
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" || calls < 2 || f.Executor.CanAcquire() {
		t.Fatalf("state=%+v err=%v calls=%d ready=%v", state, err, calls, f.Executor.CanAcquire())
	}
	var result map[string]any
	if err := json.Unmarshal(state.Result, &result); err != nil {
		t.Fatal(err)
	}
	summary, _ := result["summary"].(string)
	if environmentStop(t, state) || strings.Contains(summary, "実行環境の問題") || !strings.Contains(summary, "--- FAIL: TestBroken") {
		t.Fatalf("通常の不合格として終わっていません: %s %s", state.View, state.Result)
	}
	r, _ := f.Executor.Store.Load(1)
	var saved TestResult
	if json.Unmarshal(r.Test, &saved) != nil || saved.Unavailable {
		t.Fatal(string(r.Test))
	}
}

func TestMergeEnvironmentHoldWithoutDeniedTestRefusesAsOrdinaryFailure(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Branch = "chat/merge-held-host"
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", "-b", f.Branch)
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("feature\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "feature.txt")
	f.git(source, "commit", "-m", "feature")
	f.git(source, "push", f.bare, f.Branch)
	f.head = f.git(source, "rev-parse", "HEAD")
	f.pull = true
	f.Executor.Config.Repositories[0].TestCommand = []string{"/bin/sh", "-c", "echo '--- FAIL: TestBroken'; exit 1"}
	if err := f.Executor.recordEnvironmentHold(&os.PathError{Op: "open", Path: "/private/cache", Err: os.ErrPermission}); err != nil {
		t.Fatal(err)
	}
	r := monitorRequest{ID: "126-aabbccdd", Kind: "merge_manual", Repo: "owner/project", Payload: encode(map[string]any{"number": 1, "head": f.head})}
	result, err := f.Executor.mergeRequestedPull(context.Background(), &requestProgress{Request: r, Stage: "claimed"})
	if err != nil {
		t.Fatal(err)
	}
	message, _ := result["message"].(string)
	if !strings.Contains(message, "固定テストが不合格です") || !strings.Contains(message, "--- FAIL: TestBroken") || strings.Contains(message, "実行環境の問題") || f.merged || f.Executor.CanAcquire() {
		t.Fatalf("通常の不合格として断っていません: %v", result)
	}
}

// Applying an edit request (a dependency add) runs in the same sandbox; a refusal there is this run's own evidence.
func TestDeniedDependencyAddStopsAsEnvironmentWithoutRepair(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	pnpm := filepath.Join(t.TempDir(), "pnpm")
	writeFixture(t, pnpm, "#!/bin/sh\necho \"EACCES: permission denied, mkdir '/outside/store'\" >&2\nexit 1\n")
	if err := os.Chmod(pnpm, 0700); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.PNPM = pnpm
	base := f.Executor.Call
	calls := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := base(ctx, req)
		if !req.Review {
			calls++
			answer.JSON = reportWith("addDependencies", []any{map[string]any{"name": "left-pad", "version": "1.3.0", "dev": false}})
		}
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "failed" || calls != 1 || !environmentStop(t, state) || f.Executor.CanAcquire() {
		t.Fatalf("依存追加の拒否を環境問題として止めていません: state=%+v err=%v calls=%d ready=%v", state, err, calls, f.Executor.CanAcquire())
	}
}

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/nativetest"
	"net/url"
	"strings"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
)

// The native test contract lives in nativetest; these names keep the
// existing uses in this package.
type (
	nativeTarget       = nativetest.Target
	nativeResult       = nativetest.Result
	nativeRequestState = nativetest.RequestState
)

func nativeID(repo string, p nativeTarget, version string, attempt ...string) string {
	return nativetest.ID(repo, p, version, attempt...)
}
func nativeMergedTree(ctx context.Context, w *gitwork.Workspace, base, head string) (string, error) {
	if !gitwork.ValidSHA(base) || !gitwork.ValidSHA(head) {
		return "", errors.New("native testのSHAが不正です")
	}
	tree, err := w.Config.MergeTree(ctx, w.Tree, base, head)
	if errors.Is(err, gitwork.ErrConflict) {
		// Not the job's follow conflict; keep it a plain publication hold.
		return "", errors.New("マージ予定treeを計算できません（競合しています）")
	}
	if err != nil {
		return "", fmt.Errorf("マージ予定treeを計算できません: %w", err)
	}
	if !gitwork.ValidSHA(tree) {
		return "", errors.New("マージ予定treeを計算できません（treeのSHAが不正です）")
	}
	return tree, nil
}
func (e *Executor) nativePolicy(ctx context.Context, repo, host string) error {
	return nativetest.Policy(ctx, e.API, e.Config, repo, host)
}

// nativeGate is shared by Issue publication and manual PR merge. All failures
// become a publication hold rather than another implementation/repair attempt.
func (e *Executor) nativeGate(ctx context.Context, repo string, pr int64, w *gitwork.Workspace, head, base string, fence func(context.Context) error, attempt ...string) error {
	configured, err := e.Config.Repository(repo)
	if err != nil {
		return err
	}
	if configured.NativeTestHost == "" {
		return nil
	}
	hold := func(err error) error {
		return &releaseHold{"review_held", "Appleネイティブ検証を確認できないためマージを保留します: " + clip(err.Error(), 500), map[string]any{"releasePending": true}}
	}
	tree, err := nativeMergedTree(ctx, w, base, head)
	if err != nil {
		return hold(err)
	}
	p := nativeTarget{Host: configured.NativeTestHost, Head: head, Base: base, Tree: tree}
	if err = e.nativePolicy(ctx, repo, p.Host); err != nil {
		return hold(err)
	}
	pull, err := e.Gitea.Pull(ctx, repo, pr)
	if err != nil {
		return hold(err)
	}
	if pull.Merged || pull.State != "open" || pull.Head.SHA != head || pull.Base.SHA != base {
		return hold(errors.New("公開済みPRのhead/baseが一致しません"))
	}
	id := nativeID(repo, p, e.Config.Version, append([]string{e.Config.Host}, attempt...)...)
	route := "/api/v1/requests/" + url.PathEscape(id)
	complete := false
	defer func() {
		if !complete {
			cancelCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = e.API.Call(cancelCtx, "POST", route+"/cancel", map[string]any{}, nil)
		}
	}()
	// The deterministic ID also recovers an ambiguous create response. Never
	// substitute success for an unavailable request or submit a second ID.
	var existing struct {
		Request *nativeRequestState `json:"request"`
	}
	err = e.API.Call(ctx, "GET", route, nil, &existing)
	if err != nil {
		// Only an explicit 404 permits initial submission.
		if !isNativeNotFound(err) {
			return hold(err)
		}
		if err = fence(ctx); err != nil {
			return hold(err)
		}
		if err = e.API.Call(ctx, "POST", "/api/v1/requests", map[string]any{"id": id, "kind": "native_test", "repo": repo, "payload": p}, nil); err != nil {
			return hold(err)
		}
	}
	waitCtx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	for {
		if err = fence(waitCtx); err != nil {
			return hold(err)
		}
		if err = e.nativePolicy(waitCtx, repo, p.Host); err != nil {
			return hold(err)
		}
		var out struct {
			Request *nativeRequestState `json:"request"`
		}
		if err = e.API.Call(waitCtx, "GET", route, nil, &out); err != nil {
			return hold(err)
		}
		if out.Request == nil {
			return hold(errors.New("native test依頼がありません"))
		}
		r := out.Request
		var actual nativeTarget
		if r.ID != id || r.Kind != "native_test" || r.Repo != repo || json.Unmarshal(r.Payload, &actual) != nil || actual != p {
			return hold(errors.New("native test依頼が一致しません"))
		}
		switch r.Status {
		case "cancelled", "canceled", "expired", "failed":
			return hold(errors.New("native test依頼が失効しました"))
		}
		if r.Result != nil {
			result := r.Result
			switch result.Status {
			case "failed":
				return hold(errors.New(nativeFailureReason(result.Message)))
			case "unrunnable":
				return hold(errors.New("native testを実行できませんでした"))
			case "interrupted":
				return hold(errors.New("native testが中断されました"))
			case "passed":
			default:
				return hold(errors.New("native testの結果が不明です"))
			}
			var mismatches []string
			if result.Head != head {
				mismatches = append(mismatches, "head")
			}
			if result.Base != base {
				mismatches = append(mismatches, "base")
			}
			if result.Tree != tree {
				mismatches = append(mismatches, "tree")
			}
			if len(mismatches) != 0 {
				return hold(errors.New("native testの" + strings.Join(mismatches, "/") + "が一致しません"))
			}
			if err = fence(waitCtx); err != nil {
				return hold(err)
			}
			if err = e.nativePolicy(waitCtx, repo, p.Host); err != nil {
				return hold(err)
			}
			if err = w.Fetch(waitCtx); err != nil {
				return hold(err)
			}
			current, fetchErr := w.Base(waitCtx)
			if fetchErr != nil || current != base {
				return hold(errors.New("検証中にbaseが更新されました"))
			}
			pull, err = e.Gitea.Pull(waitCtx, repo, pr)
			if err != nil {
				return hold(err)
			}
			if pull.Head.SHA != head || pull.Base.SHA != base || pull.Merged || pull.State != "open" {
				return hold(errors.New("検証中にPRが更新されました"))
			}
			complete = true
			return nil
		}
		switch r.Status {
		case "cancelled", "canceled", "expired", "failed", "completed":
			return hold(errors.New("native testが結果なしで終了しました"))
		}
		select {
		case <-waitCtx.Done():
			return hold(waitCtx.Err())
		case <-time.After(5 * time.Second):
		}
	}
}

func isNativeNotFound(err error) bool {
	var e *intake.APIError
	return errors.As(err, &e) && e.Status == 404
}

func (e *Executor) addNativeTestGate(repo string, who map[string]any) {
	var r Repository
	for _, candidate := range e.Config.Repositories {
		if strings.EqualFold(candidate.Name, repo) {
			r = candidate
			break
		}
	}
	if r.NativeTestHost != "" {
		who["nativeTestGate"] = map[string]any{"version": 1, "host": r.NativeTestHost, "configurationVersion": e.Config.NativeTestVersions[r.NativeTestHost]}
	}
}

// nativeFailureReason exposes only a fixed stage name, never runner diagnostics.
func nativeFailureReason(message string) string {
	stages := []struct{ suffix, name string }{
		{"Flutterの依存関係を取得できません。", "Flutterの依存関係"},
		{"独自Lintパッケージの検証に失敗しました。", "独自Lintパッケージ"},
		{"Flutterの解析に失敗しました。", "Flutterの解析"},
		{"アプリの独自Lintに失敗しました。", "アプリの独自Lint"},
		{"Flutterのテストに失敗しました。", "Flutterのテスト"},
		{"iPhoneシミュレーター向けのビルドに失敗しました。", "iPhoneシミュレーター向けのビルド"},
	}
	for _, stage := range stages {
		if strings.HasSuffix(strings.TrimRight(message, "\r\n"), stage.suffix) {
			return "native testが不合格です（" + stage.name + "）"
		}
	}
	return "native testが不合格です"
}

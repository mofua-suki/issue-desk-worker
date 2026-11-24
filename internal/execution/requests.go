package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/requeststore"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Request progress is kept by requeststore; these names keep the existing
// uses in this package.
type (
	monitorRequest  = requeststore.Request
	requestProgress = requeststore.Progress
)

func (e *Executor) requests() requeststore.Store { return requeststore.Under(e.Config.RuntimeDir) }
func (e *Executor) saveRequest(p *requestProgress) error {
	return e.requests().Save(p)
}
func (e *Executor) loadRequest(r monitorRequest) (*requestProgress, error) {
	return e.requests().Load(r)
}

func (e *Executor) requestRepos() []string {
	repos := []string{}
	if e.Config.TestOnly() {
		return repos
	}
	for _, r := range e.Config.Repositories {
		for name, hosts := range e.Config.Assignments.Implementers {
			if strings.EqualFold(name, r.Name) && slices.Contains(hosts, e.Config.Host) {
				repos = append(repos, r.Name)
				break
			}
		}
	}
	return repos
}

// ジョブと同じ直列実行枠で処理し、結果の応答を失っても同じ保存済み結果を返す。
// A notice that cannot be sent yet is reported with the outcome but does not
// keep the requests waiting; it is retried on the next cycle.
func (e *Executor) ProcessRequestsFor(ctx context.Context, eligible []string) error {
	return e.ProcessRequestsWithFullTestFor(ctx, eligible, true, nil)
}

func (e *Executor) ProcessRequestsWithFullTestFor(ctx context.Context, eligible []string, allowFullTest bool, startFullTest requeststore.StartFullTest) error {
	notices := e.recoverNotifications(ctx)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if notices != nil {
		notices = fmt.Errorf("未送信の通知を送れません（次の周期で再試行します）: %w", notices)
	}
	kinds := []string{"merge_manual", "pr_create", "pr_close"}
	if allowFullTest {
		kinds = append(kinds, "full_test")
	}
	return errors.Join(notices, e.processRequests(ctx, eligible, kinds, false, startFullTest))
}

// ProcessBusyRequestsFor は、ジョブが動いている最中のリポジトリや枠が埋まったPCでも処理する依頼を受ける。
// 手動マージは依頼者が割り込みより先を求めているので待たせない。マージは依頼ごとの作業ツリーで検証し、
// リポジトリに1つのマージの番を取ってから行う。動いているジョブの取り込み直しは許容する。
// PR作成はブランチ指定だけで、パッチ指定のPR作成・固定テストはジョブと同じ直列の枠(ProcessRequestsFor)で処理する。
func (e *Executor) ProcessBusyRequestsFor(ctx context.Context, busy []string) error {
	return e.processRequests(ctx, busy, []string{"merge_manual", "pr_create"}, true, nil)
}
func (e *Executor) processRequests(ctx context.Context, eligible []string, kinds []string, busy bool, startFullTest requeststore.StartFullTest) error {
	next := &e.requestsNext
	if busy {
		next = &e.busyRequestsNext
	}
	if time.Now().Before(*next) {
		return nil
	}
	*next = time.Now().Add(15 * time.Second)
	repos := []string{}
	for _, repo := range e.requestRepos() {
		if slices.Contains(eligible, repo) {
			repos = append(repos, repo)
		}
	}
	if len(repos) == 0 {
		return nil
	}
	// A failure in one kind is kept and the next kind is still tried, so one
	// stuck request does not hold back the others.
	var failures []error
	for _, kind := range kinds {
		q := url.Values{"kind": []string{kind}, "repo": repos}
		var out struct {
			Request *monitorRequest `json:"request"`
		}
		if err := e.API.Call(ctx, "GET", "/api/v1/requests/next?"+q.Encode(), nil, &out); err != nil {
			failures = append(failures, fmt.Errorf("%sの依頼を受け取れません: %w", kind, err))
			continue
		}
		if out.Request == nil {
			continue
		}
		r := *out.Request
		if !gitwork.ValidRequestID(r.ID) || r.Kind != kind || !slices.Contains(repos, r.Repo) {
			failures = append(failures, fmt.Errorf("受付からの%sの依頼%sの対象が不正です", kind, clip(r.ID, 80)))
			continue
		}
		progress, err := e.loadRequest(r)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if busy && kind == "pr_create" && len(progress.Result) == 0 {
			var p prPayload
			if json.Unmarshal(r.Payload, &p) != nil || p.Patch != "" {
				// 作業ツリーを使う依頼は、ジョブの終わりまで待ちへ戻す。
				return errors.Join(append(failures, e.releaseRequestIgnored(ctx, r.ID))...)
			}
		}
		if len(progress.Result) == 0 {
			if kind == "full_test" && startFullTest != nil && startFullTest(r.Repo,
				func(ctx context.Context) (func(context.Context) error, error) {
					result, err := e.fullTestRequest(ctx, r)
					if errors.Is(err, errRequestReleased) {
						return nil, nil
					}
					if err := e.saveRequestResult(progress, result, err); err != nil {
						return nil, err
					}
					return func(ctx context.Context) error { return e.sendRequestResult(ctx, progress) }, nil
				},
			) {
				return errors.Join(failures...)
			}
			var result map[string]any
			switch kind {
			case "pr_create":
				result, err = e.createRequestedPullProgress(ctx, progress)
			case "pr_close":
				result, err = e.closeRequestedPull(ctx, r)
			case "merge_manual":
				result, err = e.mergeRequestedPull(ctx, progress)
			case "full_test":
				result, err = e.fullTestRequest(ctx, r)
			}
			if errors.Is(err, errRequestReleased) {
				return errors.Join(failures...)
			}
			if err := e.saveRequestResult(progress, result, err); err != nil {
				failures = append(failures, err)
				continue
			}
			if kind == "merge_manual" && result["status"] == "merged" {
				// The saved result is what a resume reports, so the PR is not re-read by head ref.
				var p struct {
					Number int64 `json:"number"`
				}
				if json.Unmarshal(r.Payload, &p) == nil {
					e.deleteMergedBranch(ctx, r.Repo, p.Number, progress.Branch)
				}
			}
		}
		if err := e.sendRequestResult(ctx, progress); err != nil {
			failures = append(failures, err)
			continue
		}
		return errors.Join(failures...)
	}
	return errors.Join(failures...)
}
func (e *Executor) saveRequestResult(progress *requestProgress, result map[string]any, runErr error) error {
	if runErr != nil {
		return fmt.Errorf("依頼%sを処理できません: %w", progress.Request.ID, runErr)
	}
	progress.Result = encode(result)
	progress.Stage = "result"
	if err := e.saveRequest(progress); err != nil {
		return fmt.Errorf("依頼%sの結果を保存できません: %w", progress.Request.ID, err)
	}
	return nil
}

func (e *Executor) sendRequestResult(ctx context.Context, progress *requestProgress) error {
	if err := e.API.Call(ctx, "POST", fmt.Sprintf("/api/v1/requests/%s/result", progress.Request.ID), map[string]any{"result": progress.Result}, nil); err != nil {
		return fmt.Errorf("依頼%sの結果を送れません: %w", progress.Request.ID, err)
	}
	return nil
}

func (e *Executor) releaseRequestIgnored(ctx context.Context, id string) error {
	if err := e.releaseRequest(ctx, id); err != nil && !errors.Is(err, errRequestReleased) {
		return err
	}
	return nil
}
func refusal(message string) map[string]any {
	return map[string]any{"status": "refused", "message": message}
}

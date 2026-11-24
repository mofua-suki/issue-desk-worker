package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"strings"
	"time"
)

func agyAnswerState(a providers.Answer) (string, bool) {
	var result struct {
		Status       string          `json:"status"`
		Prerequisite json.RawMessage `json:"prerequisiteMissing"`
	}
	_ = json.Unmarshal(a.JSON, &result)
	missing := hasJSON(result.Prerequisite)
	return result.Status, missing
}
func (e *Executor) antigravityChanged(ctx context.Context, r *journal.Record) (bool, error) {
	out, err := e.gitConfig().Local(ctx, r.Tree, 10*time.Second, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	if out.Code != 0 {
		return false, errors.New("Antigravityの作業ツリーの変更を確認できません")
	}
	return strings.TrimSpace(out.Out) != "", nil
}
func (e *Executor) switchAntigravityDenial(ctx context.Context, d Descriptor, r *journal.Record, detail string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if switchedAntigravity(r) {
		return nil
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	r.ProviderSwitch = &journal.ProviderSwitch{From: "antigravity", To: "claude", Cause: "denial", Reason: "Antigravityで想定外の読み取り拒否がありました。保護設定を広げずClaudeへ引き継ぎます", At: at, Failures: []journal.ProviderFailure{{Kind: "denial", At: at, Generation: d.Generation, Detail: failureTail(e.redact(detail))}}}
	r.Session = ""
	return e.Store.Save(r)
}
func (e *Executor) continueAntigravity(ctx context.Context, d Descriptor, r *journal.Record, prompt string, files []string) (providers.Answer, error) {
	if err := ctx.Err(); err != nil {
		return providers.Answer{}, err
	}
	if err := e.flushUsage(ctx, r); err != nil {
		return providers.Answer{}, err
	}
	if err := e.checkImplementationUsage(ctx, d, r, true, true); err != nil {
		return providers.Answer{}, fmt.Errorf("実行継続の使用量確認で停止しました: %w", err)
	}
	return e.callAIOnce(ctx, d, r, prompt, false, files)
}
func agyDenialSwitchEligible(ctx context.Context, a providers.Answer, err error) bool {
	var quota *providers.UsageLimit
	return ctx.Err() == nil && !a.Transient && !errors.As(err, &quota) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
func (e *Executor) handleAntigravityAnswer(ctx context.Context, d Descriptor, r *journal.Record, a providers.Answer, callErr error) (bool, error) {
	kinds, err := e.recordAntigravityDenied(d, r, a.Denied)
	if err != nil {
		return false, err
	}
	status, missing := agyAnswerState(a)
	if kinds.Inside+kinds.Pathless > 0 && agyDenialSwitchEligible(ctx, a, callErr) && (callErr != nil || status == "failed" && !missing) {
		detail := "Antigravityの想定外の読み取り拒否"
		if callErr != nil {
			detail += "\n" + callErr.Error()
		}
		detail += "\n" + string(a.JSON)
		return true, e.switchAntigravityDenial(ctx, d, r, detail)
	}
	return false, nil
}

// A rejection never widens AGY permissions. One same-conversation reminder precedes a no-change failure.
func (e *Executor) callAI(ctx context.Context, d Descriptor, r *journal.Record, prompt string, review bool, files []string) (providers.Answer, error) {
	if review || d.reportOnly || implementationProvider(d, r) != "antigravity" {
		return e.callAIOnce(ctx, d, r, prompt, review, files)
	}
	hold, err := e.antigravityHold(r)
	if err != nil {
		return providers.Answer{}, err
	}
	if hold != nil {
		if err = e.switchAntigravityDenial(ctx, d, r, "このPCの同じAntigravity版は保留中です: "+hold.Version+" / "+hold.Job); err != nil {
			return providers.Answer{}, err
		}
		return e.callAIOnce(ctx, d, r, prompt, false, files)
	}
	answer, callErr := e.callAIOnce(ctx, d, r, prompt, false, files)
	if r.Provider != "antigravity" {
		return answer, callErr
	}
	if ctx.Err() != nil {
		return answer, ctx.Err()
	}
	switched, err := e.handleAntigravityAnswer(ctx, d, r, answer, callErr)
	if err != nil {
		return answer, err
	}
	if switched {
		return e.continueAntigravity(ctx, d, r, prompt, files)
	}
	status, missing := agyAnswerState(answer)
	if callErr != nil || len(answer.Denied) == 0 || status == "question" || missing {
		return answer, callErr
	}
	changed, err := e.antigravityChanged(ctx, r)
	if err != nil {
		return answer, err
	}
	if changed {
		return answer, nil
	}
	reminder := prompt + "\nコントローラーの確認では作業ツリーに変更がありません。コマンドは拒否されました。コマンドを使わず、ファイルの読み取り・検索・編集だけで依頼を実装し、同じJSON契約で結果を返してください。"
	answer, callErr = e.continueAntigravity(ctx, d, r, reminder, files)
	if ctx.Err() != nil {
		return answer, ctx.Err()
	}
	switched, err = e.handleAntigravityAnswer(ctx, d, r, answer, callErr)
	if err != nil {
		return answer, err
	}
	if switched {
		return e.continueAntigravity(ctx, d, r, prompt, files)
	}
	if callErr != nil {
		return answer, callErr
	}
	status, missing = agyAnswerState(answer)
	if status == "question" || missing {
		return answer, nil
	}
	changed, err = e.antigravityChanged(ctx, r)
	if err != nil {
		return answer, err
	}
	if !changed {
		var result map[string]any
		if json.Unmarshal(answer.JSON, &result) != nil {
			return answer, errors.New("Antigravityの回答を確認できません")
		}
		result["status"] = "failed"
		result["summary"] = fmt.Sprint(result["summary"]) + "\nコマンド拒否後に同じ会話で確認しましたが作業ツリーに変更がありません。"
		answer.JSON = encode(result)
	}
	return answer, nil
}

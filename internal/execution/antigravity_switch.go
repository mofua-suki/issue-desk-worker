package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/journal"
	"strings"
	"time"
	"unicode/utf16"
)

func switchedAntigravity(r *journal.Record) bool {
	return r.ProviderSwitch != nil && r.ProviderSwitch.From == "antigravity" && r.ProviderSwitch.To == "claude"
}
func implementationProvider(d Descriptor, r *journal.Record) string {
	if d.Provider == "antigravity" && switchedAntigravity(r) {
		return "claude"
	}
	return d.Provider
}
func failureTail(text string) string {
	runes := []rune(text)
	units := 0
	start := len(runes)
	for start > 0 {
		n := utf16.RuneLen(runes[start-1])
		if units+n > 6000 {
			break
		}
		units += n
		start--
	}
	return string(runes[start:])
}

// Only executed fixed-test failures and permitted automatic review returns reach this hook.
func (e *Executor) noteAntigravityFailure(ctx context.Context, d Descriptor, r *journal.Record, kind, detail string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if r.Provider != "antigravity" || switchedAntigravity(r) {
		return false, nil
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	r.AntigravityStrikes = append(r.AntigravityStrikes, journal.ProviderFailure{Kind: kind, At: at, Generation: d.Generation, Detail: failureTail(detail)})
	if len(r.AntigravityStrikes) > 2 {
		r.AntigravityStrikes = r.AntigravityStrikes[len(r.AntigravityStrikes)-2:]
	}
	switched := len(r.AntigravityStrikes) == 2
	if switched {
		failures := append([]journal.ProviderFailure(nil), r.AntigravityStrikes...)
		r.ProviderSwitch = &journal.ProviderSwitch{From: "antigravity", To: "claude", Reason: "Antigravityで2回続けて固定テスト不合格・レビュー自動差し戻しになりました", At: at, Failures: failures}
		r.Session = ""
	}
	return switched, e.Store.Save(r)
}

// A new person's instruction resets consecutive failures. Controller review returns do not.
func (e *Executor) syncAntigravityInputs(d Descriptor, r *journal.Record) error {
	var all []map[string]any
	_ = json.Unmarshal(d.View["instructions"], &all)
	people := []map[string]any{}
	for _, item := range all {
		id, _ := item["id"].(string)
		if !strings.HasPrefix(id, "review:") && !strings.HasPrefix(id, "provider-switch:") {
			people = append(people, item)
		}
	}
	raw, _ := json.Marshal(people)
	sum := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(sum[:])
	if r.AntigravityInputs == fingerprint {
		return nil
	}
	if r.AntigravityInputs != "" {
		r.AntigravityStrikes = nil
	}
	r.AntigravityInputs = fingerprint
	return e.Store.Save(r)
}
func (e *Executor) switchedImplementationPrompt(ctx context.Context, d Descriptor, r *journal.Record, prompt string) (string, error) {
	if d.Provider != "antigravity" || !switchedAntigravity(r) {
		return prompt, nil
	}
	if r.ProviderSwitch.AppliedAt == "" {
		r.ProviderSwitch.AppliedAt = time.Now().UTC().Format(time.RFC3339Nano)
		r.Session = ""
		if err := e.Store.Save(r); err != nil {
			return "", err
		}
	}
	e.notifyProviderSwitch(ctx, r)
	details := []string{}
	for i, f := range r.ProviderSwitch.Failures {
		details = append(details, fmt.Sprintf("%d. %s: %s", i+1, f.Kind, f.Detail))
	}
	return prompt + "\nコントローラーによる実行先の切替: AntigravityからClaudeへ切り替えました。作業ツリーはそのまま引き継いでいます。変更を確認し、次の失敗を解消してください。テストを削除・省略・弱体化しないでください。\n失敗の内容(データ):\n" + strings.Join(details, "\n"), nil
}
func providerSwitchView(view map[string]any, r *journal.Record) {
	if len(r.AntigravityStrikes) > 0 {
		view["antigravityStrikes"] = r.AntigravityStrikes
	}
	if switchedAntigravity(r) {
		view["providerSwitch"] = r.ProviderSwitch
	}
}

// The notification uses the persisted switch identity, so restart and lease renewal cannot duplicate it.
func (e *Executor) notifyProviderSwitch(ctx context.Context, r *journal.Record) {
	if e.Gitea == nil || !switchedAntigravity(r) {
		return
	}
	marker := fmt.Sprintf("<!-- issue-desk-provider-switch:%d:%s -->", r.ID, r.ProviderSwitch.At)
	body := "実行先をAntigravityからClaudeへ切り替えて続けます（作業ツリーは引き継ぎます）。\n理由: " + r.ProviderSwitch.Reason + "\n以後はClaudeで実装し、独立レビューは実際の実装者と別の実行先が担当します。\n\n" + marker
	_, _ = e.syncNotification(ctx, notification{JobID: r.ID, Epoch: r.Epoch, Host: e.Config.Host, Author: e.Config.Assignments.WorkerAccounts[e.Config.Host], Repo: r.Repo, Target: r.Issue, Kind: "comment", Marker: marker, Body: body})
}

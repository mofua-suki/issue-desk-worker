// Package readiness owns the host-check wire contract shared by heartbeat and its startup gate.
package readiness

import "time"

type Item struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}
type Report struct {
	At              time.Time
	SettingsVersion string
	Items           []Item
}

var labels = map[string]string{"server": "受付への接続", "config": "必須の設定", "giteaToken": "Giteaのトークンと本人", "giteaScopes": "Giteaのトークンの権限", "volume": "状態のフォルダー", "cli": "CLIの版と契約認証", "isolation": "資格情報と状態の隔離", "recovery": "保存状態の復旧", "webhook": "Webhookの待受", "runtime": "実行器の準備"}

func (r *Report) Record(name string, ok bool) {
	label := labels[name]
	if label == "" {
		name = "runtime"
		label = labels[name]
	}
	detail := "確認しました"
	if !ok {
		detail = "確認できません。起動時の診断を確認してください"
	}
	r.Items = append(r.Items, Item{Name: name, Label: label, OK: ok, Detail: detail})
}
func (r Report) Value() map[string]any {
	ok := len(r.Items) > 0
	items := make([]any, 0, len(r.Items))
	for _, i := range r.Items {
		ok = ok && i.OK
		items = append(items, map[string]any{"name": i.Name, "label": i.Label, "ok": i.OK, "detail": i.Detail})
	}
	return map[string]any{"at": r.At.UTC().Format(time.RFC3339Nano), "ok": ok, "items": items, "settingsVersion": r.SettingsVersion}
}

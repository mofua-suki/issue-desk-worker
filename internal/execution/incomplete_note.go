package execution

import (
	"encoding/json"
	"strings"
)

// withIncomplete は、AIが未完了で返した理由・完了部分・残作業を保留の要約に足す。
// 要約は finish で2000文字に切られるので、本文を先に切り詰めて足した部分が残るようにする。
func withIncomplete(summary string, raw json.RawMessage) string {
	var incomplete struct {
		Reason    string   `json:"reason"`
		Completed []string `json:"completed"`
		Remaining []string `json:"remaining"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &incomplete) != nil {
		return summary
	}
	reason := strings.TrimSpace(incomplete.Reason)
	completed := joinItems(incomplete.Completed)
	remaining := joinItems(incomplete.Remaining)
	if reason == "" && completed == "" && remaining == "" {
		return summary
	}
	var note strings.Builder
	if reason != "" {
		note.WriteString("\n未完了の理由: " + clip(reason, 500))
	}
	if completed != "" {
		note.WriteString("\n完了した部分: " + clip(completed, 400))
	}
	if remaining != "" {
		note.WriteString("\n残りの作業: " + clip(remaining, 400))
	}
	return clip(summary, 600) + note.String()
}
func joinItems(items []string) string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return strings.Join(out, "、")
}

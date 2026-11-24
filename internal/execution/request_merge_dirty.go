package execution

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"issue-desk-worker/internal/gitwork"
)

func (e *Executor) dirtyMergeRefusal(ctx context.Context, w *gitwork.Workspace) (map[string]any, error) {
	const prefix = "固定テスト・整形の後に作業ツリーが変わりました"
	items, err := w.WorktreeStatus(ctx)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return refusal(prefix + "\n変更一覧を取得できません"), nil
	}
	lines := make([]string, 0, len(items))
	for _, item := range items {
		paths := make([]string, len(item.Paths))
		for i, path := range item.Paths {
			paths[i] = strconv.QuoteToGraphic(e.redact(path))
		}
		if len(paths) == 2 {
			paths[0], paths[1] = paths[1], paths[0]
		}
		lines = append(lines, item.Status+" "+strings.Join(paths, " -> "))
	}
	// Reserve the omission count before adding each complete entry.
	message := prefix
	shown := 0
	for _, line := range lines {
		if shown == 20 {
			break
		}
		remainder := len(lines) - shown - 1
		suffix := ""
		if remainder > 0 {
			suffix = fmt.Sprintf("\nほか%d件", remainder)
		}
		if utf8.RuneCountInString(message)+1+utf8.RuneCountInString(line)+utf8.RuneCountInString(suffix) > 2000 {
			break
		}
		message += "\n" + line
		shown++
	}
	if omitted := len(lines) - shown; omitted > 0 {
		message += fmt.Sprintf("\nほか%d件", omitted)
	}
	return refusal(message), nil
}

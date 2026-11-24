package execution

import (
	"context"
	"errors"
	"issue-desk-worker/internal/journal"
	"os"
	"slices"
	"strings"
)

func (e *Executor) recordFileAction(r *journal.Record, name, action, status string) error {
	if e.Store == nil {
		return errors.New("controllerの反映履歴を保存できません")
	}
	r.ControllerChanges = append(r.ControllerChanges, encode(map[string]string{"path": name, "action": action, "status": status}))
	return e.Store.Save(r)
}
func (e *Executor) applyDeleteRequests(ctx context.Context, r *journal.Record, names []string) error {
	if len(names) > 200 {
		return errors.New("削除要求の件数が上限を超えました")
	}
	type item struct {
		name   string
		target *safeTarget
	}
	plan := []item{}
	defer func() {
		for _, entry := range plan {
			if entry.target != nil {
				entry.target.close()
			}
		}
	}()
	seen := map[string]bool{}
	for _, name := range names {
		clean, err := cleanControllerPath(name)
		if err != nil {
			return err
		}
		if r.ControllerAllowed != nil && !strings.HasPrefix(clean, ".claude-staged/") && !slices.Contains(r.ControllerAllowed, clean) {
			return errors.New("削除要求が競合解消の許容範囲外です")
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		target, err := openSafeTarget(r.Tree, clean, false)
		if errors.Is(err, os.ErrNotExist) {
			plan = append(plan, item{clean, nil})
			continue
		}
		if err != nil {
			return err
		}
		plan = append(plan, item{clean, target})
	}
	for _, entry := range plan {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.target == nil || entry.target.info == nil {
			if err := e.recordFileAction(r, entry.name, "delete", "missing"); err != nil {
				return err
			}
			continue
		}
		if err := e.recordFileAction(r, entry.name, "delete", "attempted"); err != nil {
			return err
		}
		if err := entry.target.remove(); err != nil {
			_ = e.recordFileAction(r, entry.name, "delete", "failed")
			return err
		}
		if err := e.recordFileAction(r, entry.name, "delete", "applied"); err != nil {
			return err
		}
	}
	return nil
}

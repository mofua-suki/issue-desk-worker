package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// narrowScope reports a distinct actual set once for each current scope during
// this execution. A later widening can therefore be narrowed again.
func (e *Executor) narrowScope(ctx context.Context, d *Descriptor, epoch int64, paths []string, fence func(context.Context) error, push func(context.Context) error, r *journal.Record, attempted map[string]bool) error {
	if len(paths) == 0 || len(paths) > 200 {
		return ctx.Err()
	}
	actual := append([]string(nil), paths...)
	scope := append([]string(nil), d.Scope...)
	sort.Strings(actual)
	sort.Strings(scope)
	if equalPathSets(actual, scope) {
		return ctx.Err()
	}
	for _, path := range actual {
		if path == "*" || !scopeContains(scope, path) {
			return ctx.Err()
		}
	}
	key, _ := json.Marshal([][]string{scope, actual})
	if attempted[string(key)] {
		return ctx.Err()
	}
	if err := fence(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	attempted[string(key)] = true
	// Publish the snapshot before the intake evaluates the request. Some intake
	// implementations may inspect the branch HEAD when resolving overlap.
	if err := push(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fenceErr := fence(ctx); fenceErr != nil {
			return fenceErr
		}
		fmt.Fprintln(os.Stderr, "変更範囲の縮小前のPushに失敗しました")
		return e.recordScopeNarrow(r, false)
	}
	if err := fence(ctx); err != nil {
		return err
	}
	var response struct {
		Result string     `json:"result"`
		Job    Descriptor `json:"job"`
	}
	err := e.API.Call(ctx, "POST", fmt.Sprintf("/api/v1/jobs/%d/scope", d.ID), map[string]any{"epoch": epoch, "actual": actual}, &response)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		var apiErr *intake.APIError
		if errors.As(err, &apiErr) && apiErr.LeaseLost() {
			return err
		}
		fmt.Fprintf(os.Stderr, "変更範囲の縮小に失敗しました: %v\n", err)
		return e.recordScopeNarrow(r, false)
	}
	if response.Result != "granted" {
		fmt.Fprintf(os.Stderr, "変更範囲の縮小が許可されませんでした: %q\n", response.Result)
		return e.recordScopeNarrow(r, false)
	}
	if response.Job.ID != d.ID {
		return errors.New("変更範囲の応答が対象ジョブと一致しません")
	}
	for _, path := range actual {
		if !scopeContains(response.Job.Scope, path) {
			fmt.Fprintln(os.Stderr, "変更範囲の縮小応答に実際の変更が含まれません")
			return e.recordScopeNarrow(r, false)
		}
	}
	if equalPathSets(scope, response.Job.Scope) {
		fmt.Fprintln(os.Stderr, "変更範囲は縮小されませんでした")
		return e.recordScopeNarrow(r, false)
	}
	d.Scope = response.Job.Scope
	return e.recordScopeNarrow(r, true)
}

func equalPathSets(a, b []string) bool {
	unique := func(paths []string) map[string]struct{} {
		set := make(map[string]struct{}, len(paths))
		for _, path := range paths {
			set[path] = struct{}{}
		}
		return set
	}
	left, right := unique(a), unique(b)
	if len(left) != len(right) {
		return false
	}
	for path := range left {
		if _, ok := right[path]; !ok {
			return false
		}
	}
	return true
}

// Counts are stored per job in the existing protected journal, across restarts.
func (e *Executor) recordScopeNarrow(r *journal.Record, success bool) error {
	if success {
		r.ScopeNarrowed++
	} else {
		r.ScopeNarrowFailed++
	}
	return e.Store.Save(r)
}

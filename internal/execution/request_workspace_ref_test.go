package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"issue-desk-worker/internal/process"
)

// 枝の有無を確かめるGitが失敗した（出力の上限超え・時間切れ・起動失敗）ときは、終了コード0を「枝がある」と読まず、その失敗を返す。
func TestRequestWorkspaceReturnsFailedRefCheck(t *testing.T) {
	for _, tc := range []struct{ name, sub, ref string }{
		{"remote", "rev-parse", "refs/remotes/origin/chat/new-branch"},
		{"local", "show-ref", "refs/heads/chat/new-branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			real := f.Executor.Config.Git
			wrapper := filepath.Join(f.root, "git-ref-check-fails")
			// 対象の枝を確かめる呼び出しだけを、出力の上限（process の既定）を十分に超える量で失敗させる。
			script := "#!/bin/sh\nsub=; ref=\nfor a; do case \"$a\" in " + tc.sub + ") sub=1;; " + tc.ref + ") ref=1;; esac; done\n" +
				"[ -n \"$sub\" ] && [ -n \"$ref\" ] && exec head -c 64000000 /dev/zero\nexec '" + real + "' \"$@\"\n"
			if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			f.Executor.Config.Git = wrapper
			_, err := f.Executor.requestWorkspace(context.Background(), monitorRequest{ID: "126-aabbccdd", Repo: "owner/project"}, "main", "chat/new-branch")
			if !errors.Is(err, process.ErrOutputLimit) {
				t.Fatalf("枝の確認の失敗を返していません: %v", err)
			}
		})
	}
}

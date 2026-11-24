package execution

import (
	"fmt"
	"syscall"
	"testing"

	"issue-desk-worker/internal/process"
)

// 起動の失敗が容量不足・クォータ超過の errno なら、終了コードや stderr を見ずに容量不足とする。
func TestSandboxOutOfSpaceFromErrno(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"ENOSPC":   {fmt.Errorf("書き込めません: %w", syscall.ENOSPC), true},
		"EDQUOT":   {fmt.Errorf("書き込めません: %w", syscall.EDQUOT), true},
		"ほかのerrno": {fmt.Errorf("書き込めません: %w", syscall.EACCES), false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := sandboxOutOfSpace(process.Result{}, tc.err); got != tc.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

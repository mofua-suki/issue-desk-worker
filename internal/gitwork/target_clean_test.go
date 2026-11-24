package gitwork

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFastForwardTargetChecksStatus(t *testing.T) {
	cases := []struct {
		name, status   string
		dirty, failure bool
	}{
		{"clean", "exit 0", false, false},
		{"tracked", `printf ' M tracked\0'`, true, false},
		{"untracked", `printf '?? new/file\0'`, true, false},
		{"status failure", `echo 'secret https://example.invalid' >&2; exit 7`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "merged")
			script := filepath.Join(dir, "git")
			body := `#!/bin/sh
while [ "$1" = "-c" ]; do shift 2; done
if [ "$1" = status ]; then
  [ "$#" = 4 ] && [ "$2" = --porcelain ] && [ "$3" = -z ] && [ "$4" = --untracked-files=all ] || exit 9
  ` + tc.status + `
elif [ "$1" = merge ]; then
  [ "$2" = --ff-only ] && [ "$3" = refs/remotes/origin/codex/test ] || exit 9
  : > '` + marker + `'
else
  exit 9
fi
`
			if err := os.WriteFile(script, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			w := &Workspace{Config: Config{Git: script}, Tree: dir, Branch: "codex/test"}
			err := w.FastForwardTarget(context.Background())
			if errors.Is(err, ErrWorkspaceUncommitted) != tc.dirty || (err != nil) != (tc.dirty || tc.failure) {
				t.Fatalf("unexpected error: %v", err)
			}
			if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "example.invalid")) {
				t.Fatalf("status detail leaked: %v", err)
			}
			_, statErr := os.Stat(marker)
			if (statErr == nil) != (!tc.dirty && !tc.failure) {
				t.Fatalf("merge marker: %v", statErr)
			}
		})
	}
}

func TestCheckTargetCleanExecutionFailure(t *testing.T) {
	w := &Workspace{Config: Config{Git: filepath.Join(t.TempDir(), "missing-git")}, Tree: t.TempDir()}
	err := w.FastForwardTarget(context.Background())
	if err == nil || errors.Is(err, ErrWorkspaceUncommitted) {
		t.Fatalf("execution failure was not distinguished: %v", err)
	}
}

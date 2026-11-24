package execution

import (
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/tempdir"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"issue-desk-worker/internal/process"
)

func removeSandboxTemp(root string) error { return tempdir.Remove(root) }

// logSandboxTempRemoval records a temporary directory that could not be
// removed; the startup sweep retries it.
func logSandboxTempRemoval(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "一時領域を消せませんでした（次の起動で再試行します）: %s\n", cause.Kind(err))
	}
}

// sweepStaleTemps removes the per-run temporary directories (fixed tests,
// AI sessions, isolation checks) that a crashed or killed run left. It runs
// only at startup, while this process holds the journal lock and nothing
// else uses the runtime directory.
func sweepStaleTemps(runtimeDir string) error {
	dirs := []string{filepath.Join(runtimeDir, "workspaces")}
	if tmpfs := os.Getenv("ISSUE_DESK_SANDBOX_TMPFS_DIR"); filepath.IsAbs(tmpfs) {
		dirs = append(dirs, tmpfs)
	}
	return tempdir.Sweep(dirs, []string{".test-*", ".sandbox-*", "_isolation_check-*"})
}

// A candidate must be a private tmpfs directory with enough initial headroom.
// Each invocation gets its own directory; it is never a persistent cache root.
const sandboxTempMinFree = 16 << 20

func makeSandboxTemp(runtimeDir, tree, tokenFile string, allowTmpfs bool) (string, bool, error) {
	candidate := os.Getenv("ISSUE_DESK_SANDBOX_TMPFS_DIR")
	if allowTmpfs && filepath.IsAbs(candidate) {
		candidate, err := filepath.EvalSymlinks(candidate)
		if err == nil && sandboxTempSeparate(candidate, runtimeDir, tree, tokenFile) {
			var stat syscall.Statfs_t
			if syscall.Statfs(candidate, &stat) == nil && uint64(stat.Type) == 0x01021994 && stat.Bsize > 0 && stat.Bavail >= (sandboxTempMinFree+uint64(stat.Bsize)-1)/uint64(stat.Bsize) && stat.Ffree > 0 {
				if temp, err := os.MkdirTemp(candidate, ".test-"); err == nil {
					return temp, true, nil
				}
			}
		}
	}
	temp, err := os.MkdirTemp(filepath.Join(runtimeDir, "workspaces"), ".test-")
	return temp, false, err
}

func sandboxTempSeparate(candidate, runtimeDir, tree, tokenFile string) bool {
	protected := []string{tree, filepath.Dir(tokenFile), filepath.Join(runtimeDir, "state"), filepath.Join(runtimeDir, "control"), os.Getenv("CODEX_HOME"), os.Getenv("CLAUDE_CONFIG_DIR")}
	if home, err := os.UserHomeDir(); err == nil {
		protected = append(protected, home)
	}
	for _, path := range protected {
		if path == "" || path == "." {
			continue
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			resolved, err = filepath.Abs(path)
			if err != nil {
				return false
			}
		}
		if containsPath(candidate, resolved) || containsPath(resolved, candidate) {
			return false
		}
	}
	return true
}

// containsPath reports whether child is parent itself or lies beneath it.
func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func sandboxOutOfSpace(out process.Result, err error) bool {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return true
	}
	if out.Interrupted || out.Code == 0 {
		return false
	}
	// Child tools report errno through stderr, not through structured process exits.
	// Only terminal diagnostics qualify; echoed stdout and prose mentions do not.
	for _, line := range strings.Split(strings.ToLower(out.Err), "\n") {
		line = strings.TrimSpace(line)
		if line == "no space left on device" || strings.HasSuffix(line, ": no space left on device") || line == "disk quota exceeded" || strings.HasSuffix(line, ": disk quota exceeded") {
			return true
		}
	}
	return false
}

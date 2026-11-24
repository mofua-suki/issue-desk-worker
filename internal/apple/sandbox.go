package apple

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/process"
	"issue-desk-worker/internal/tempdir"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (a *Runner) profile(tree, home string, policies ...fleetconfig.Repository) string {
	q := strconv.Quote
	// Read only system SDK/tool installations and the request's private checkout.
	// No general home-directory access or network; credentials and control state
	// stay outside the allowed paths, even when a test knows their filenames.
	roots := []string{"/System", "/Library/Developer", "/Library/Frameworks", "/Library/Apple", "/Applications", "/usr", "/bin", "/sbin", "/opt/homebrew", "/private/var/db/dyld", "/dev", tree, home}
	var policy fleetconfig.Repository
	if len(policies) > 0 {
		policy = policies[0]
	}
	roots = append(roots, policy.NativeToolRoots...)
	var b strings.Builder
	// / の一覧とファイルの属性（中身ではない）は読めないと /bin/sh 自体が起動直後に異常終了する（macOSで確認）。
	b.WriteString("(version 1) (deny default) (allow process-exec) (allow process-fork) (allow signal (target children)) (allow sysctl-read) (allow mach-lookup) (allow ipc-posix*) (allow file-read-metadata) (allow file-read* (literal \"/\"))\n")
	for _, root := range roots {
		b.WriteString("(allow file-read* (subpath " + q(root) + "))\n")
	}
	if policy.NativeNetwork {
		b.WriteString("(allow network*)\n")
	}
	writes := append([]string{tree, home}, darwinUserDirs()...)
	for _, root := range writes {
		b.WriteString("(allow file-write* (subpath " + q(root) + "))\n")
	}
	b.WriteString(xcodeRules())
	b.WriteString("(allow file-write* (literal \"/dev/null\"))\n")
	// Explicit deny wins even if a bootstrap token was placed below an allowed SDK.
	for _, p := range []string{a.Bootstrap.ServerTokenFile, a.Config.WorkerTokenFile, filepath.Join(a.Root, "control")} {
		abs, err := filepath.Abs(p)
		if err == nil {
			if resolved, e := filepath.EvalSymlinks(abs); e == nil {
				abs = resolved
			}
			b.WriteString("(deny file-read* file-write* (subpath " + q(abs) + "))\n")
		}
	}
	return b.String()
}

func (a *Runner) runSandbox(ctx context.Context, tree string, command []string, timeout time.Duration, policies ...fleetconfig.Repository) (process.Result, error) {
	if len(command) == 0 {
		return process.Result{}, errors.New("testCommandがありません")
	}
	home, err := os.MkdirTemp(a.Root, testHomePrefix)
	if err != nil {
		return process.Result{}, err
	}
	defer removeTemp(home)
	// Resolve /var -> /private/var before writing a sandbox profile.
	tree, _ = filepath.EvalSymlinks(tree)
	home, _ = filepath.EvalSymlinks(home)
	var policy fleetconfig.Repository
	if len(policies) > 0 {
		policy = policies[0]
	}
	roots, err := a.toolRoots(policy)
	if err != nil {
		return process.Result{}, err
	}
	// Copies live in the disposable HOME, never hardlink host SDK files.
	copies, err := copyAppleSDKs(roots, home)
	if err != nil {
		return process.Result{}, err
	}
	aliases := policy.NativeToolRoots
	policy.NativeToolRoots = copies
	command = append([]string(nil), command...)
	for i, arg := range command {
		for j, root := range roots {
			for _, prefix := range []string{root, filepath.Clean(aliases[j])} {
				if filepath.IsAbs(arg) && containsPath(prefix, arg) {
					rel, _ := filepath.Rel(prefix, arg)
					command[i] = filepath.Join(copies[j], rel)
				}
			}
		}
	}
	path := "/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin"
	shims, err := writeXcodeShims(home)
	if err != nil {
		return process.Result{}, err
	}
	path = shims + ":" + path
	for _, root := range copies {
		path = filepath.Join(root, "bin") + ":" + path
	}
	cache := filepath.Join(home, "pub-cache")
	if policy.Name != "" {
		digest := fmt.Sprintf("%x", sha256.Sum256([]byte(policy.Name)))
		cache = filepath.Join(a.Root, "repository-cache", digest)
		if err = os.MkdirAll(cache, 0700); err != nil {
			return process.Result{}, err
		}
	}
	profile := a.profile(tree, home, policy)
	profile += "\n(allow file-read* file-write* (subpath " + strconv.Quote(cache) + "))\n"
	args := []string{"-p", profile, "--"}
	args = append(args, command...)
	return process.Run(ctx, a.sandbox, args, process.Options{KillGroupOnExit: true, Dir: tree, Timeout: timeout, Env: map[string]string{"PATH": path, "PUB_CACHE": cache, "XDG_CACHE_HOME": filepath.Join(home, "cache"), "XDG_CONFIG_HOME": filepath.Join(home, ".config"), "HOME": home, "TMPDIR": home, "CODEX_HOME": "", "CLAUDE_CONFIG_DIR": "", "GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}})
}

func (a *Runner) toolRoots(policy fleetconfig.Repository) ([]string, error) {
	var roots []string
	home, _ := os.UserHomeDir()
	for _, raw := range policy.NativeToolRoots {
		reject := func() ([]string, error) { return nil, fmt.Errorf("SDKパスを許可できません: %s", raw) }
		if !filepath.IsAbs(raw) {
			return reject()
		}
		root, err := filepath.EvalSymlinks(raw)
		if err != nil {
			return reject()
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return reject()
		}
		for _, protected := range []string{home, a.Root, a.Config.RuntimeDir, a.Bootstrap.ServerTokenFile, a.Config.WorkerTokenFile} {
			if protected == "" {
				continue
			}
			protected, _ = filepath.Abs(protected)
			if resolved, e := filepath.EvalSymlinks(protected); e == nil {
				protected = resolved
			}
			if containsPath(root, protected) || containsPath(filepath.Join(home, ".ssh"), root) || containsPath(filepath.Join(home, ".config"), root) {
				return reject()
			}
		}
		if root == "/" || root == "/Users" || root == "/home" || root == "/tmp" || root == "/usr" || root == "/opt" || root == "/Applications" || filepath.Dir(root) == "/Users" || filepath.Dir(root) == "/home" {
			return reject()
		}
		for _, name := range []string{".ssh", ".aws", ".codex", ".claude", ".docker", ".config", "Library/Keychains"} {
			sensitive := filepath.Join(home, name)
			if containsPath(root, sensitive) || containsPath(sensitive, root) {
				return reject()
			}
		}
		for _, control := range []string{a.Root, a.Config.RuntimeDir} {
			if control != "" {
				resolved := control
				if p, e := filepath.EvalSymlinks(control); e == nil {
					resolved = p
				}
				if containsPath(resolved, root) {
					return reject()
				}
			}
		}
		if bin, e := filepath.EvalSymlinks(filepath.Join(root, "bin")); e == nil && !containsPath(root, bin) {
			return reject()
		}
		cache := filepath.Join(root, "bin", "cache")
		if resolved, e := filepath.EvalSymlinks(cache); e == nil && !containsPath(root, resolved) {
			return reject()
		}
		if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				resolved, err := filepath.EvalSymlinks(path)
				if err != nil || !containsPath(root, resolved) {
					return cause.New("SDK外のsymlink", err)
				}
			}
			return nil
		}); err != nil {
			return reject()
		}
		roots = append(roots, root)
	}
	return roots, nil
}

func tempPatterns() []string {
	return []string{probePrefix + "*", checkoutPrefix + "*", testHomePrefix + "*"}
}

// removeTemp deletes a per-run directory, logging one it could not remove;
// the next start retries it.
func removeTemp(dir string) {
	if err := tempdir.Remove(dir); err != nil {
		fmt.Fprintf(os.Stderr, "一時領域を消せませんでした（次の起動で再試行します）: %v\n", err)
	}
}

// copyAppleSDKs preserves internal symlinks, rebasing absolute links into the
// copy. Every regular file has its own inode; writable copies cannot poison the
// installed toolchain or the next request.
func copyAppleSDKs(roots []string, home string) ([]string, error) {
	var copies []string
	for i, root := range roots {
		dest := filepath.Join(home, fmt.Sprintf("sdk-%d", i))
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			target := filepath.Join(dest, rel)
			info, err := entry.Info()
			if err != nil {
				return err
			}
			switch {
			case entry.IsDir():
				return os.Mkdir(target, info.Mode().Perm()|0700)
			case entry.Type()&os.ModeSymlink != 0:
				resolved, err := filepath.EvalSymlinks(path)
				if err != nil {
					return err
				}
				if !containsPath(root, resolved) {
					return errors.New("SDK外のsymlink")
				}
				link, err := os.Readlink(path)
				if err != nil {
					return err
				}
				if filepath.IsAbs(link) {
					inside, _ := filepath.Rel(root, resolved)
					link = filepath.Join(dest, inside)
				}
				return os.Symlink(link, target)
			case info.Mode().IsRegular():
				in, err := os.Open(path)
				if err != nil {
					return err
				}
				defer in.Close()
				out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm()|0600)
				if err != nil {
					return err
				}
				_, copyErr := io.Copy(out, in)
				closeErr := out.Close()
				if copyErr != nil {
					return copyErr
				}
				return closeErr
			default:
				return errors.New("SDKに特殊ファイルがあります")
			}
		})
		if err != nil {
			return nil, err
		}
		copies = append(copies, dest)
	}
	return copies, nil
}

func containsPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

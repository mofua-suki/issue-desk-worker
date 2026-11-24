package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
)

const mergedHead = "1111111111111111111111111111111111111111"

func mergedBranchFixture(t *testing.T, merged bool, headRef, branchTip string, branchStatus int, openPulls string) (*Executor, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.EscapedPath())
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/owner/project/pulls/7":
			fmt.Fprintf(w, `{"number":7,"merged":%t,"head":{"ref":%q,"sha":%q,"repo":{"full_name":"owner/project"}},"base":{"ref":"main","repo":{"full_name":"owner/project"}}}`, merged, headRef, mergedHead)
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/owner/project/pulls" && r.URL.Query().Get("state") == "open":
			io.WriteString(w, openPulls)
		case r.Method == "GET" && r.URL.Path == "/api/v1/repos/owner/project":
			io.WriteString(w, `{"default_branch":"main"}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/repos/owner/project/branches/"):
			if branchStatus != 200 {
				w.WriteHeader(branchStatus)
				return
			}
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, branchTip)
		default:
			t.Errorf("unexpected Gitea request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(server.Close)
	g, err := gitea.New(server.URL, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	git := filepath.Join(root, "git")
	if err := os.WriteFile(git, []byte("#!/bin/sh\n: > \"$0-started\"\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(git + "-started"); !os.IsNotExist(err) {
			t.Errorf("Git must not start: stat marker: %v", err)
		}
	})
	return &Executor{Gitea: g, Config: Configuration{Fleet: Fleet{Git: git, RuntimeDir: root, GiteaURL: server.URL}}}, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

func TestDeleteMergedBranch(t *testing.T) {
	for _, name := range []string{"at merged head", "pushed after GET"} {
		t.Run(name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			e := f.Executor
			const branch = "chat/topic"
			const ref = "refs/heads/" + branch
			git := func(args ...string) string {
				return f.git(f.root, append([]string{"--git-dir", f.bare}, args...)...)
			}
			head := git("rev-parse", "refs/heads/main")
			git("update-ref", ref, head)
			tip := head
			race := name == "pushed after GET"
			if race {
				tip = git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", head+"^{tree}", "-p", head, "-m", "New branch tip")
			}
			mirror := filepath.Join(e.Config.RuntimeDir, "control", "repos", "owner__project.git")
			f.git(f.root, "init", "--bare", mirror)
			f.git(f.root, "--git-dir", mirror, "remote", "add", "origin", f.server.URL+"/owner/project.git")
			f.git(f.root, "--git-dir", mirror, "config", "http.proxy", "")
			f.GiteaOverride = func(w http.ResponseWriter, r *http.Request) bool {
				switch r.Method + " " + r.URL.Path {
				case "GET /api/v1/repos/owner/project/pulls/7":
					fmt.Fprintf(w, `{"number":7,"merged":true,"head":{"ref":%q,"sha":%q,"repo":{"full_name":"owner/project"}},"base":{"ref":"main","repo":{"full_name":"owner/project"}}}`, branch, head)
				case "GET /api/v1/repos/owner/project/pulls":
					io.WriteString(w, `[{"number":7,"head":{"ref":"chat/topic","repo":{"full_name":"owner/project"}},"base":{"ref":"main"}}]`)
				case "GET /api/v1/repos/owner/project/branches/" + branch:
					if race {
						// Advance the ref after selecting the GET snapshot and before Git pushes.
						cmd := exec.CommandContext(r.Context(), "git", "--git-dir", f.bare, "update-ref", ref, tip, head)
						if out, err := cmd.CombinedOutput(); err != nil {
							t.Errorf("advance ref: %v: %s", err, out)
							w.WriteHeader(http.StatusInternalServerError)
							return true
						}
					}
					fmt.Fprintf(w, `{"commit":{"id":%q}}`, head)
				default:
					return false
				}
				return true
			}
			step, cleanupErr := e.removeMergedBranch(t.Context(), "owner/project", 7, branch)
			if race {
				var pushErr *gitwork.PushError
				if !errors.As(cleanupErr, &pushErr) {
					t.Fatalf("expected Git to reject the deletion: %s: %v", step, cleanupErr)
				}
			} else if cleanupErr != nil {
				t.Fatalf("delete merged branch: %s: %v", step, cleanupErr)
			}
			out, err := exec.Command("git", "--git-dir", f.bare, "rev-parse", "--verify", "--quiet", ref).Output()
			if !race {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 {
					t.Fatalf("branch should be absent: %s (%v)", out, err)
				}
			} else if err != nil || strings.TrimSpace(string(out)) != tip {
				t.Fatalf("branch tip = %s (%v), want %s", out, err, tip)
			}
			if got := git("rev-parse", "refs/heads/main"); got != head {
				t.Fatalf("main changed to %s", got)
			}
		})
	}
}

func TestDeleteMergedBranchKeepsUnsafeBranches(t *testing.T) {
	other := `[{"number":8,"head":{"ref":"chat/topic","repo":{"full_name":"owner/project"}},"base":{"ref":"release"}}]`
	stacked := `[{"number":9,"head":{"ref":"chat/next","repo":{"full_name":"owner/project"}},"base":{"ref":"chat/topic"}}]`
	for name, c := range map[string]struct {
		merged       bool
		headRef, tip string
		branch       string
		branchStatus int
		openPulls    string
		wantRequests int
	}{
		"not merged":           {false, "chat/topic", mergedHead, "chat/topic", 200, `[]`, 1},
		"pushed after merge":   {true, "chat/topic", "2222222222222222222222222222222222222222", "chat/topic", 200, `[]`, 4},
		"different head ref":   {true, "chat/other", mergedHead, "chat/topic", 200, `[]`, 1},
		"default branch":       {true, "main", mergedHead, "main", 200, `[]`, 2},
		"already deleted":      {true, "chat/topic", mergedHead, "chat/topic", 404, `[]`, 4},
		"branch read failure":  {true, "chat/topic", mergedHead, "chat/topic", 500, `[]`, 4},
		"open PR from branch":  {true, "chat/topic", mergedHead, "chat/topic", 200, other, 3},
		"open PR onto branch":  {true, "chat/topic", mergedHead, "chat/topic", 200, stacked, 3},
		"open PR read failure": {true, "chat/topic", mergedHead, "chat/topic", 200, `not json`, 3},
		"issue branch":         {true, "codex/issue-7", mergedHead, "codex/issue-7", 200, `[]`, 0},
	} {
		t.Run(name, func(t *testing.T) {
			e, calls := mergedBranchFixture(t, c.merged, c.headRef, c.tip, c.branchStatus, c.openPulls)
			e.deleteMergedBranch(context.Background(), "owner/project", 7, c.branch)
			if got := calls(); len(got) != c.wantRequests {
				t.Fatalf("Gitea requests = %v, want %d requests", got, c.wantRequests)
			}
		})
	}
}

func TestTransientGiteaRetriesOnlyClearableFailures(t *testing.T) {
	for name, c := range map[string]struct {
		err  error
		want bool
	}{
		"unreachable":  {gitea.ErrUnreachable, true},
		"deadline":     {context.DeadlineExceeded, true},
		"server error": {&gitea.Error{Status: 502}, true},
		"timeout":      {&gitea.Error{Status: 408}, true},
		"rate limited": {&gitea.Error{Status: 429}, true},
		"forbidden":    {&gitea.Error{Status: 403}, false},
		"conflict":     {&gitea.Error{Status: 409}, false},
		"invalid JSON": {errors.New("GiteaのJSON応答が不正です"), false},
		"too many PRs": {errors.New("開いたPRが多すぎます"), false},
	} {
		if got := transientGitea(c.err); got != c.want {
			t.Error(name, got)
		}
	}
}

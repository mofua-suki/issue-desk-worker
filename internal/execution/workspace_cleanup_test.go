package execution

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
)

func cleanupFixture(t *testing.T, handler http.HandlerFunc) (*WorkspaceCleaner, func(int64) *journal.Record) {
	t.Helper()
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	g, err := gitea.New(server.URL, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{Store: store, Gitea: g, Config: Configuration{Fleet: Fleet{RuntimeDir: store.Root, Git: "must-not-run"}}}
	return &WorkspaceCleaner{Executor: e}, func(id int64) *journal.Record {
		r := &journal.Record{ID: id, Repo: "owner/project", Issue: id, Stage: "done", FinalReported: true, Generation: 3, Tree: filepath.Join(store.Root, "workspaces", "owner__project", fmt.Sprintf("job-%d", id))}
		if err := os.MkdirAll(r.Tree, 0700); err != nil {
			t.Fatal(err)
		}
		if err := store.Save(r); err != nil {
			t.Fatal(err)
		}
		return r
	}
}

func TestWorkspaceCleanupSelectionAndMissingTrees(t *testing.T) {
	var calls atomic.Int32
	c, record := cleanupFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"number":1,"state":"open"}`)
	})
	for i, stage := range []string{"implement", "held", "failed", "stopped", "done", "done", "done", "done"} {
		r := record(int64(i + 1))
		r.Stage = stage
		switch i {
		case 4:
			r.FinalReported = false
		case 5:
			r.Repo = "busy/project"
		case 6:
			if err := os.Remove(r.Tree); err != nil {
				t.Fatal(err)
			}
		case 7:
			r.Tree = filepath.Dir(r.Tree)
		}
		if err := c.Executor.Store.Save(r); err != nil {
			t.Fatal(err)
		}
	}
	c.Cleanup(context.Background(), []string{"owner/project"})
	if calls.Load() != 0 {
		t.Fatal("ineligible/missing tree queried Gitea", calls.Load())
	}
	for id := int64(1); id <= 6; id++ {
		r, _ := c.Executor.Store.Load(id)
		if _, err := os.Stat(r.Tree); err != nil {
			t.Fatal("ineligible tree lost", id, err)
		}
	}
}

func TestWorkspaceCleanupMatchesEligibleRepositoryIgnoringCase(t *testing.T) {
	var calls atomic.Int32
	c, record := cleanupFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/repos/owner/project/issues/1" {
			t.Error("journal repository spelling changed", r.URL.Path)
		}
		io.WriteString(w, `{"number":1,"state":"open"}`)
	})
	r := record(1)
	c.Cleanup(context.Background(), []string{"Owner/Project"})
	if calls.Load() != 1 {
		t.Fatal("eligible repository case prevented Issue check", calls.Load())
	}
	if _, err := os.Stat(r.Tree); err != nil {
		t.Fatal("open Issue lost its tree", err)
	}
}

func TestWorkspaceCleanupMergedBranchFailures(t *testing.T) {
	for name, c := range map[string]struct {
		status     int // Returned to PR reads (pulls/).
		pushStatus int // Returned to Git receive-pack discovery.
		dropPush   bool
		denyDelete bool
		sibling    bool
		corrupt    bool
		keepTree   bool
		keepBranch bool
	}{
		"PR read unavailable":     {status: 503, keepTree: true, keepBranch: true},
		"PR read rate limited":    {status: 429, keepTree: true, keepBranch: true},
		"push connection lost":    {dropPush: true, keepTree: true, keepBranch: true},
		"push method not allowed": {pushStatus: http.StatusMethodNotAllowed, keepBranch: true},
		"branch delete forbidden": {denyDelete: true, keepBranch: true},
		"unfinished sibling job":  {sibling: true, keepBranch: true},
		"unreadable journal file": {corrupt: true, keepBranch: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			if err := os.WriteFile(f.Executor.Config.WorkerTokenFile, []byte("fixture-token"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" {
				t.Fatal(state, err)
			}
			if err := f.Executor.Reported(f.Job.ID, f.Job.LeaseEpoch); err != nil {
				t.Fatal(err)
			}
			r, err := f.Executor.Store.Load(f.Job.ID)
			if err != nil || !cleanupComplete(r) || r.PR < 1 {
				t.Fatal("production completion not eligible", r, err)
			}
			if c.sibling {
				next := journal.Record{ID: r.ID + 1, Repo: r.Repo, Issue: r.Issue, Generation: r.Generation + 1, Stage: "implement", Branch: r.Branch, PR: r.PR}
				if err := f.Executor.Store.Save(&next); err != nil {
					t.Fatal(err)
				}
			}
			if c.corrupt {
				if err := os.WriteFile(filepath.Join(f.Executor.Store.Root, "state", fmt.Sprintf("job-%d.json", r.ID+99)), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if c.denyDelete {
				f.git(f.root, "--git-dir", f.bare, "config", "receive.denyDeletes", "true")
			}
			var dropPush atomic.Bool
			dropPush.Store(c.dropPush)
			f.GiteaOverride = func(w http.ResponseWriter, req *http.Request) bool {
				if req.URL.Path == "/owner/project.git/info/refs" && req.URL.Query().Get("service") == "git-receive-pack" {
					if c.pushStatus != 0 {
						w.WriteHeader(c.pushStatus)
						return true
					}
					if !dropPush.Load() {
						return false
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusInternalServerError)
						return true
					}
					_ = conn.Close()
					return true
				}
				if c.status == 0 || !strings.Contains(req.URL.Path, "/pulls/") {
					return false
				}
				w.WriteHeader(c.status)
				return true
			}
			f.closed = true
			cleaner := &WorkspaceCleaner{Executor: f.Executor}
			cleaner.Cleanup(ctx, []string{r.Repo})
			_, statErr := os.Stat(r.Tree)
			if (c.keepTree && statErr != nil) || (!c.keepTree && !os.IsNotExist(statErr)) {
				t.Fatal("tree stat", statErr, "want tree kept", c.keepTree)
			}
			branch := exec.Command("git", "--git-dir", f.bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+r.Branch).Run() == nil
			if branch != c.keepBranch {
				t.Fatal("branch kept", branch, "want", c.keepBranch)
			}
			if c.dropPush {
				dropPush.Store(false)
				cleaner.Cleanup(ctx, []string{r.Repo})
				if _, err := os.Stat(r.Tree); !os.IsNotExist(err) {
					t.Fatal("tree survived successful retry", err)
				}
				if refs := f.git(f.root, "--git-dir", f.bare, "for-each-ref", "refs/heads/"+r.Branch); refs != "" {
					t.Fatal("branch survived successful retry", refs)
				}
			}
		})
	}
}

func TestWorkspaceCleanupHTTPFailuresKeepTreeAndSanitizeLogs(t *testing.T) {
	for _, kind := range []string{"open", "http", "redirect", "empty", "invalid", "missing-state", "null", "state-type", "unknown-state", "wrong-number", "missing-number", "connection"} {
		t.Run(kind, func(t *testing.T) {
			c, record := cleanupFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/api/v1/repos/owner/project/issues/1" {
					t.Error("wrong issue request", r.Method, r.URL.Path)
				}
				switch kind {
				case "open":
					io.WriteString(w, `{"number":1,"state":"open"}`)
				case "http":
					w.WriteHeader(500)
					io.WriteString(w, `{"message":"fixture-secret-do-not-log"}`)
				case "redirect":
					w.Header().Set("Location", "/fixture-secret-do-not-log")
					w.WriteHeader(302)
				case "empty":
					w.WriteHeader(204)
				case "invalid":
					io.WriteString(w, `{"state":"closed"} trailing fixture-secret-do-not-log`)
				case "missing-state":
					io.WriteString(w, `{"number":1}`)
				case "null":
					io.WriteString(w, `null`)
				case "state-type":
					io.WriteString(w, `{"number":1,"state":true}`)
				case "unknown-state":
					io.WriteString(w, `{"number":1,"state":"CLOSED"}`)
				case "wrong-number":
					io.WriteString(w, `{"number":2,"state":"closed"}`)
				case "missing-number":
					io.WriteString(w, `{"state":"closed"}`)
				case "connection":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
				}
			})
			r := record(1)
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			stderr := os.Stderr
			os.Stderr = write
			func() { defer func() { os.Stderr = stderr }(); c.Cleanup(context.Background(), []string{r.Repo}) }()
			_ = write.Close()
			log, _ := io.ReadAll(read)
			_ = read.Close()
			if strings.Contains(string(log), "fixture-secret") {
				t.Fatal("remote response leaked")
			}
			if kind != "open" && len(log) == 0 {
				t.Fatal("failure not logged")
			}
			if _, err := os.Stat(r.Tree); err != nil {
				t.Fatal("unconfirmed tree lost", err)
			}
		})
	}
}

func TestWorkspaceCleanupLimitRotationAndCancellation(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var stop context.CancelFunc
	paths := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
	c, record := cleanupFixture(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		cancel := stop
		mu.Unlock()
		if cancel != nil {
			cancel()
		}
		w.WriteHeader(503) // Failures must also count toward the five attempts.
	})
	for id := int64(1); id <= 12; id++ {
		record(id)
	}
	for cycle := 0; cycle < 3; cycle++ {
		before := len(paths())
		c.Cleanup(context.Background(), []string{"owner/project"})
		if len(paths())-before != 5 {
			t.Fatal("cycle limit", len(paths())-before)
		}
	}
	unique := map[string]bool{}
	for _, path := range paths() {
		unique[path] = true
	}
	if len(unique) != 12 {
		t.Fatal("failed earlier rows starved later rows", unique)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := len(paths())
	c.Cleanup(ctx, []string{"owner/project"})
	if len(paths()) != before {
		t.Fatal("cancelled cycle made requests")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	mu.Lock()
	stop = cancel
	mu.Unlock()
	c.Cleanup(ctx, []string{"owner/project"})
	if len(paths()) != before+1 {
		t.Fatal("cancel not checked between records")
	}
}

func TestWorkspaceCleanupBoundsSlowGitea(t *testing.T) {
	c, record := cleanupFixture(t, func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	r := record(1)
	started := time.Now()
	c.Cleanup(context.Background(), []string{r.Repo})
	if elapsed := time.Since(started); elapsed < workspaceCleanupTimeout-time.Second || elapsed > workspaceCleanupTimeout+2*time.Second {
		t.Fatal("cycle deadline", elapsed)
	}
	if _, err := os.Stat(r.Tree); err != nil {
		t.Fatal("timed out tree lost", err)
	}
}

func TestWorkspaceCleanupProducedCompletionAndProtectedFiles(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	if err := os.WriteFile(f.Executor.Config.WorkerTokenFile, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" {
		t.Fatal(state, err)
	}
	if err := f.Executor.Reported(f.Job.ID, f.Job.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	r, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil || !cleanupComplete(r) {
		t.Fatal("production completion not eligible", r, err)
	}
	manual, err := f.Executor.requestMergeWorkspace(ctx, monitorRequest{ID: "123-aabbccdd", Repo: r.Repo}, "main", r.Branch)
	if err != nil {
		t.Fatal(err)
	}
	refs := f.git(f.root, "--git-dir", manual.Mirror, "show-ref")
	review := filepath.Join(f.Executor.Config.RuntimeDir, "workspaces", ".review", fmt.Sprint(r.ID))
	if err := os.MkdirAll(review, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(review, "keep"), []byte("review evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	protected := cleanupProtectedSnapshot(t, f.Executor)
	c := &WorkspaceCleaner{Executor: f.Executor}
	branchExists := func() bool {
		return exec.Command("git", "--git-dir", f.bare, "rev-parse", "--verify", "--quiet", "refs/heads/"+r.Branch).Run() == nil
	}
	if r.PR < 1 || !branchExists() {
		t.Fatal("merged job has no PR branch", r.PR)
	}
	c.Cleanup(ctx, []string{r.Repo}) // The Issue is still open in the Gitea fixture.
	if _, err := os.Stat(r.Tree); err != nil {
		t.Fatal("open Issue lost its tree", err)
	}
	if !branchExists() {
		t.Fatal("open Issue lost its merged branch")
	}
	f.closed = true
	c.Cleanup(ctx, []string{r.Repo})
	if _, err := os.Stat(r.Tree); !os.IsNotExist(err) {
		t.Fatal("closed completed tree retained", err)
	}
	if branchExists() {
		t.Fatal("merged branch of a closed job retained")
	}
	if got := f.git(f.root, "--git-dir", manual.Mirror, "worktree", "list", "--porcelain"); strings.Contains(got, "worktree "+r.Tree+"\n") || !strings.Contains(got, "worktree "+manual.Tree+"\n") {
		t.Fatal("incorrect Git registrations", got)
	}
	if got := f.git(f.root, "--git-dir", manual.Mirror, "show-ref"); got != refs {
		t.Fatal("mirror refs changed")
	}
	if !reflect.DeepEqual(protected, cleanupProtectedSnapshot(t, f.Executor)) {
		t.Fatal("journal, locks or credentials written")
	}
	if data, err := os.ReadFile(filepath.Join(review, "keep")); err != nil || string(data) != "review evidence" {
		t.Fatal("review evidence lost", err)
	}
}

func TestWorkspaceCleanupBatchRemovesFiveThenRemainingTrees(t *testing.T) {
	f := newPipeline(t, "approve")
	e := f.Executor
	var trees []string
	for id := int64(1); id <= 7; id++ {
		w, err := gitwork.Open(context.Background(), gitwork.Config{Git: e.Config.Git, Root: e.Config.RuntimeDir, BaseURL: e.Config.GiteaURL, TokenFile: e.Config.WorkerTokenFile, Executable: e.Executable}, "owner/project", "main", fmt.Sprintf("codex/job-%d", id), id)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Store.Save(&journal.Record{ID: id, Repo: w.Repo, Issue: 7, Tree: w.Tree, Stage: "done", FinalReported: true}); err != nil {
			t.Fatal(err)
		}
		trees = append(trees, w.Tree)
	}
	f.closed = true
	c := &WorkspaceCleaner{Executor: e}
	for _, want := range []int{5, 7} {
		c.Cleanup(context.Background(), []string{"owner/project"})
		removed := 0
		for _, tree := range trees {
			_, err := os.Lstat(tree)
			if os.IsNotExist(err) {
				removed++
			} else if err != nil {
				t.Fatal(err)
			}
		}
		if removed != want {
			t.Fatal("removed trees", removed, "want", want)
		}
	}
}

func cleanupProtectedSnapshot(t *testing.T, e *Executor) map[string]string {
	t.Helper()
	out := map[string]string{}
	read := func(path string) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		out[path] = fmt.Sprint(info.Mode(), info.ModTime().UnixNano(), string(data))
	}
	if err := filepath.WalkDir(filepath.Join(e.Config.RuntimeDir, "state"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			read(path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read(e.Config.WorkerTokenFile)
	return out
}

func TestWorkspaceCleanupRechecksJournalBeforeRemoval(t *testing.T) {
	for _, kind := range []string{"generation", "implement", "held", "failed", "stopped", "unreported", "tree", "repo", "issue", "missing", "dirty"} {
		t.Run(kind, func(t *testing.T) {
			f := newPipeline(t, "approve")
			e := f.Executor
			w, err := gitwork.Open(context.Background(), gitwork.Config{Git: e.Config.Git, Root: e.Config.RuntimeDir, BaseURL: e.Config.GiteaURL, TokenFile: e.Config.WorkerTokenFile, Executable: e.Executable}, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			r := &journal.Record{ID: 1, Repo: w.Repo, Issue: 7, Tree: w.Tree, Stage: "done", FinalReported: true}
			if err := e.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
				switch kind {
				case "generation":
					r.Generation++
				case "implement", "held", "failed", "stopped":
					r.Stage = kind
				case "unreported":
					r.FinalReported = false
				case "tree":
					r.Tree += "-other"
				case "repo":
					r.Repo = "other/project"
				case "issue":
					r.Issue++
				case "dirty":
					if err := os.WriteFile(filepath.Join(w.Tree, "README.md"), []byte("dirty"), 0600); err != nil {
						t.Error(err)
					}
				}
				if err := e.Store.Save(r); err != nil {
					t.Error(err)
				}
				if kind == "missing" {
					if err := os.Remove(filepath.Join(e.Config.RuntimeDir, "state", "job-1.json")); err != nil {
						t.Error(err)
					}
				}
				io.WriteString(out, `{"number":7,"state":"closed"}`)
			}))
			defer server.Close()
			e.Gitea, err = gitea.New(server.URL, "fixture-token")
			if err != nil {
				t.Fatal(err)
			}
			(&WorkspaceCleaner{Executor: e}).Cleanup(context.Background(), []string{w.Repo})
			if _, err := os.Stat(w.Tree); err != nil {
				t.Fatal("changed/dirty job removed", err)
			}
		})
	}
}

func TestWorkspaceCleanupSlowRemovalEndsCycle(t *testing.T) {
	f := newPipeline(t, "approve")
	e := f.Executor
	var trees []string
	for id := int64(1); id <= 2; id++ {
		w, err := gitwork.Open(context.Background(), gitwork.Config{Git: e.Config.Git, Root: e.Config.RuntimeDir, BaseURL: e.Config.GiteaURL, TokenFile: e.Config.WorkerTokenFile, Executable: e.Executable}, "owner/project", "main", fmt.Sprintf("codex/job-%d", id), id)
		if err != nil {
			t.Fatal(err)
		}
		if err := e.Store.Save(&journal.Record{ID: id, Repo: w.Repo, Issue: 7, Tree: w.Tree, Stage: "done", FinalReported: true}); err != nil {
			t.Fatal(err)
		}
		trees = append(trees, w.Tree)
	}
	realGit, err := exec.LookPath(e.Config.Git)
	if err != nil {
		t.Fatal(err)
	}
	e.Config.Git = filepath.Join(f.root, "slow-cleanup-git")
	// Match real Git except for remove latency exceeding the admission window.
	script := "#!/bin/sh\nfor arg do\n if [ \"$arg\" = remove ]; then sleep 6; fi\ndone\nexec '" + strings.ReplaceAll(realGit, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(e.Config.Git, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.closed = true
	c := &WorkspaceCleaner{Executor: e}
	for cycle := 0; cycle < 2; cycle++ {
		c.Cleanup(context.Background(), []string{"Owner/Project"})
		for i, tree := range trees {
			_, err := os.Stat(filepath.Join(tree, ".git"))
			if i <= cycle && !os.IsNotExist(err) {
				t.Fatal("slow cleanup did not finish", i, err)
			}
			if i > cycle && err != nil {
				t.Fatal("another removal started after admission deadline", i, err)
			}
		}
	}
	for _, tree := range trees {
		if _, err := os.Lstat(tree); !os.IsNotExist(err) {
			t.Fatal("partial worktree retained", err)
		}
	}
	mirror := filepath.Join(e.Config.RuntimeDir, "control", "repos", "owner__project.git")
	registered := f.git(f.root, "--git-dir", mirror, "worktree", "list", "--porcelain", "-z")
	for _, tree := range trees {
		if strings.Contains(registered, "worktree "+tree+"\x00") {
			t.Fatal("removed worktree still registered", tree)
		}
	}
}

func TestWorkspaceCleanupBoundedReads(t *testing.T) {
	c, _ := cleanupFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected issue request") })
	for i := int64(1); i <= 500; i++ {
		if err := c.Executor.Store.Save(&journal.Record{ID: i, Repo: "owner/project", Issue: i, Stage: "implement"}); err != nil {
			t.Fatal(err)
		}
	}
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := syscall.InotifyAddWatch(fd, filepath.Join(c.Executor.Store.Root, "state"), syscall.IN_OPEN); err != nil {
		t.Fatal(err)
	}
	c.Cleanup(context.Background(), []string{"owner/project"})
	count := 0
	buf := make([]byte, 65536)
	for {
		n, err := syscall.Read(fd, buf)
		if err == syscall.EAGAIN {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for off := 0; off < n; {
			size := int(binary.LittleEndian.Uint32(buf[off+12:]))
			name := strings.TrimRight(string(buf[off+16:off+16+size]), "\x00")
			if strings.HasPrefix(name, "job-") && strings.HasSuffix(name, ".json") {
				count++
			}
			off += 16 + size
		}
	}
	if count == 0 || count > workspaceCleanupScanLimit+workspaceCleanupLimit {
		t.Fatalf("opened %d records", count)
	}
}

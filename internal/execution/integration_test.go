package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/jobspec"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestFixtureCommand(t *testing.T) {
	for _, arg := range os.Args {
		if arg == "--worker-check-file" {
			data, err := os.ReadFile("feature.txt")
			if err != nil || string(data) != "accepted feature\n" {
				os.Exit(9)
			}
			os.Exit(0)
		}
	}
}

// These review results intentionally do not meet the repeated-heavy definition.
// A later approval ends the fixture so it can verify the intervening retries.
func TestReviewRetriesForSizeEmptyHoldAndBlockingMinor(t *testing.T) {
	parallelCases := make(chan struct{}, 4)
	for _, tc := range []struct {
		name, kind string
		tooLarge   bool
	}{
		{name: "size only", tooLarge: true},
		{name: "empty hold"},
		{name: "security minor", kind: "security"},
		{name: "data minor", kind: "data"},
		{name: "blocking minor", kind: "blocking"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parallelCases <- struct{}{}
			defer func() { <-parallelCases }()
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			original := f.Executor.Call
			reviews, implementations := 0, 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := original(ctx, req)
				if err != nil {
					return a, err
				}
				if !req.Review {
					implementations++
					if implementations > 1 {
						if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte(strings.Repeat("revision\n", implementations)), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return a, nil
				}
				reviews++
				if reviews <= 2 {
					var review map[string]any
					if err := json.Unmarshal(a.JSON, &review); err != nil {
						t.Fatal(err)
					}
					review["decision"] = "hold"
					review["tooLarge"] = tc.tooLarge
					if tc.kind != "" {
						review["findings"] = []string{"同じ指摘"}
						review["findingKinds"] = []string{tc.kind}
						review["findingSeverities"] = []string{"minor"}
					}
					a.JSON = encode(review)
				}
				return a, nil
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || reviews != 3 || implementations != 3 {
				t.Fatalf("state=%+v err=%v reviews=%d implementations=%d", state, err, reviews, implementations)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil || saved.ReviewHolds != 2 {
				t.Fatalf("saved=%+v err=%v", saved, err)
			}
		})
	}
}

type pipelineFixture struct {
	Notes              []map[string]any
	Comments           []map[string]any
	Reviews            []map[string]any
	NoticePosts        int
	NoticeReadStatus   int
	NoticePostStatus   int
	AckStatus          int
	PullBase           string
	CreateStatus       int
	NextRequest        *monitorRequest
	RequestKinds       []string
	RequestPosts       []string
	HideNotices        bool
	DropNoticeResponse bool
	CurrentLeaseEpoch  int64
	CurrentJobHead     string

	t                  *testing.T
	root, bare         string
	mu                 sync.Mutex
	closed             bool
	pull               bool
	merged             bool
	head               string
	API                *intake.Client
	Executor           *Executor
	Job                intake.Job
	server             *httptest.Server
	Calls              []string
	Branch             string
	Body               string
	Mode               string
	ScopeActual        func(http.ResponseWriter, []string)
	ScopeAdd           func(http.ResponseWriter, []string)
	AllowUsage         bool
	UsageLimitReason   *string
	StateStatus        int
	CurrentPolicy      *Configuration
	CurrentReviewNotes json.RawMessage
	CurrentView        map[string]json.RawMessage
	CurrentBranch      string
	PullOverride       func(*gitea.Pull)
	// GiteaOverride が真を返した要求は、API・Gitともにその応答で終える。
	GiteaOverride   func(http.ResponseWriter, *http.Request) bool
	TurnCalls       []string
	TurnResponse    func(http.ResponseWriter, *http.Request) bool
	TurnRenewStatus int
	Commands        []map[string]any
	ReceivedStates  []intake.State
	PullReads       int
	CreatePosts     int
	CreatePostDrop  bool
	MergePosts      int
	// MergeFailStatus が0でなければ、マージ要求へこの状態とMergeFailBodyを返す。MergeFailApplied ならマージしてから返す。
	MergeFailCount   int
	MergeHeads       []string
	MergeFailStatus  int
	MergeFailBody    string
	MergeFailApplied bool
	// PullFailAfterPost が0でなければ、マージ要求の後のPRの読み直しへこの状態を返す。
	PullFailAfterPost int
	HideCreated       bool
	MergeCommit       string
}

func (f *pipelineFixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	data, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("fixture git %v: %v %s", args, err, data)
	}
	return strings.TrimSpace(string(data))
}

// fixtureTestTimeoutSeconds bounds the fixture's fixed test. The fixture's own
// tests finish in well under a second; the bound only has to outlast a loaded PC,
// where a 10-second bound cut `node --test` off. Tests of the timeout set their own.
const fixtureTestTimeoutSeconds = 60

func newPipeline(t *testing.T, mode string) *pipelineFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("gitがない環境です")
	}
	f := &pipelineFixture{t: t, root: t.TempDir(), Mode: mode, Branch: "codex/issue-7-job-1", Reviews: []map[string]any{}}
	source := filepath.Join(f.root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	f.git(source, "init", "-b", "main")
	f.git(source, "config", "user.name", "Fixture")
	f.git(source, "config", "user.email", "fixture@example.invalid")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("project\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureTest := "const { test } = require('node:test'); const assert = require('node:assert/strict'); const fs = require('node:fs'); test('feature', () => assert.equal(fs.readFileSync('feature.txt', 'utf8'), 'accepted feature\\n'));\n"
	if err := os.WriteFile(filepath.Join(source, "feature.test.js"), []byte(fixtureTest), 0600); err != nil {
		t.Fatal(err)
	}
	commandTest := "const {test} = require('node:test'); const {execFileSync} = require('node:child_process'); test('configured check', () => execFileSync('/bin/sh', ['-c', process.argv[2]], {stdio:'inherit'}));\n"
	if err := os.WriteFile(filepath.Join(source, "fixture-command.test.js"), []byte(commandTest), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", ".")
	f.git(source, "commit", "-m", "Initial fixture")
	repos := filepath.Join(f.root, "repositories")
	if err := os.MkdirAll(filepath.Join(repos, "owner"), 0700); err != nil {
		t.Fatal(err)
	}
	f.bare = filepath.Join(repos, "owner", "project.git")
	f.git(f.root, "clone", "--bare", source, f.bare)
	f.git(f.root, "--git-dir", f.bare, "config", "http.receivepack", "true")
	backend := filepath.Join(f.git(f.root, "--exec-path"), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backendがない環境です")
	}
	backendHandler := &cgi.Handler{Path: backend, Root: repos, Env: []string{"GIT_PROJECT_ROOT=" + repos, "GIT_HTTP_EXPORT_ALL=1"}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		api := strings.HasPrefix(r.URL.Path, "/api/")
		if api {
			w.Header().Set("Content-Type", "application/json")
		}
		if f.GiteaOverride != nil && f.GiteaOverride(w, r) {
			return
		}
		if !api {
			backendHandler.ServeHTTP(w, r)
			return
		}
		switch {
		case r.URL.Path == "/api/v1/repos/owner/project":
			io.WriteString(w, `{"default_branch":"main","empty":false,"permissions":{"push":true}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/project/issues/7/timeline":
			io.WriteString(w, `[]`)
		case r.URL.Path == "/api/v1/repos/owner/project/issues/7":
			if r.Method == "PATCH" {
				io.WriteString(w, `{"state":"closed"}`)
			} else {
				if f.closed {
					io.WriteString(w, `{"number":7,"title":"Implement feature","state":"closed"}`)
					return
				}
				io.WriteString(w, `{"number":7,"title":"Implement feature","body":"受入条件: feature.txtを生成する","state":"open","user":{"login":"issue-desk-intake"}}`)
			}
		case r.URL.Path == "/api/v1/repos/owner/project/pulls" && r.Method == "GET":
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.pull && !f.HideCreated {
				base := "main"
				if f.PullBase != "" {
					base = f.PullBase
				}
				fmt.Fprintf(w, `[{"number":1,"state":"open","body":%q,"head":{"ref":%q,"sha":%q,"repo":{"full_name":"owner/project"}},"base":{"ref":%q,"repo":{"full_name":"owner/project"}}}]`, f.Body, f.Branch, f.head, base)
			} else {
				io.WriteString(w, `[]`)
			}
		case r.URL.Path == "/api/v1/repos/owner/project/pulls" && r.Method == "POST":
			f.mu.Lock()
			defer f.mu.Unlock()
			var created struct {
				Body string `json:"body"`
				Head string `json:"head"`
			}
			json.NewDecoder(r.Body).Decode(&created)
			f.CreatePosts++
			if f.CreateStatus != 0 {
				w.WriteHeader(f.CreateStatus)
				return
			}
			f.Body = created.Body
			f.Branch = created.Head
			f.pull = true
			f.head = f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch)
			if f.CreatePostDrop {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			fmt.Fprintf(w, `{"number":1,"body":%q,"html_url":"http://fixture/pulls/1","head":{"sha":%q,"ref":%q,"repo":{"full_name":"owner/project"}},"base":{"ref":"main"}}`, f.Body, f.head, f.Branch)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/repos/owner/project/branches/"):
			ref := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/owner/project/branches/")
			commit := f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+ref)
			fmt.Fprintf(w, `{"commit":{"id":%q}}`, commit)
		case r.URL.Path == "/api/v1/repos/owner/project/issues/7/comments" || r.URL.Path == "/api/v1/repos/owner/project/pulls/1/reviews":
			f.mu.Lock()
			defer f.mu.Unlock()
			isReview := strings.Contains(r.URL.Path, "/reviews")
			if r.Method == "GET" {
				if f.NoticeReadStatus != 0 {
					w.WriteHeader(f.NoticeReadStatus)
					return
				}
				if f.HideNotices {
					io.WriteString(w, `[]`)
				} else if isReview {
					_ = json.NewEncoder(w).Encode(f.Reviews)
				} else {
					_ = json.NewEncoder(w).Encode(f.Comments)
				}
				break
			}
			var value map[string]any
			_ = json.NewDecoder(r.Body).Decode(&value)
			f.NoticePosts++
			if f.NoticePostStatus != 0 {
				w.WriteHeader(f.NoticePostStatus)
				return
			}
			value["id"] = f.NoticePosts
			value["user"] = map[string]any{"login": "worker"}
			if isReview {
				f.Reviews = append(f.Reviews, value)
			} else {
				f.Comments = append(f.Comments, value)
			}
			if f.DropNoticeResponse {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			_ = json.NewEncoder(w).Encode(value)
		case r.URL.Path == "/api/v1/repos/owner/project/pulls/1/merge":
			f.mu.Lock()
			defer f.mu.Unlock()
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["do"] != "fast-forward-only" || body["head_commit_id"] != f.head {
				t.Error("マージ対象が固定されていません")
			}
			f.MergeHeads = append(f.MergeHeads, fmt.Sprint(body["head_commit_id"]))
			failMerge := f.MergeFailStatus != 0 && (f.MergeFailCount == 0 || f.MergePosts < f.MergeFailCount)
			if failMerge && !f.MergeFailApplied {
				f.MergePosts++
				w.WriteHeader(f.MergeFailStatus)
				_, _ = w.Write([]byte(f.MergeFailBody))
				return
			}
			f.git(f.root, "--git-dir", f.bare, "merge-base", "--is-ancestor", "refs/heads/main", f.head)
			f.git(f.root, "--git-dir", f.bare, "update-ref", "refs/heads/main", f.head)
			f.merged = true
			f.MergePosts++
			if failMerge {
				w.WriteHeader(f.MergeFailStatus)
				_, _ = w.Write([]byte(f.MergeFailBody))
				return
			}
			w.WriteHeader(204)
		case r.URL.Path == "/api/v1/repos/owner/project/pulls/1":
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.PullFailAfterPost != 0 && f.MergePosts > 0 {
				w.WriteHeader(f.PullFailAfterPost)
				return
			}
			base := f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/main")
			if f.pull {
				f.head = f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch)
			}
			p := gitea.Pull{Number: 1, State: "open", Mergeable: true, Merged: f.merged, MergeCommitSHA: map[bool]string{true: f.head}[f.merged]}
			p.Head.SHA = f.head
			p.Head.Ref = f.Branch
			p.Head.Repo.FullName = "owner/project"
			p.Base.SHA = base
			p.Base.Ref = "main"
			p.Base.Repo.FullName = "owner/project"
			f.PullReads++
			if f.MergeCommit != "" && p.Merged {
				p.MergeCommitSHA = f.MergeCommit
			}
			if f.PullOverride != nil {
				f.PullOverride(&p)
			}
			_ = json.NewEncoder(w).Encode(p)
		default:
			t.Errorf("予期しないGitea経路: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(f.server.Close)
	runtimeDir := filepath.Join(f.root, "runtime")
	store, err := journal.Open(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	fakeCodex := filepath.Join(f.root, "codex")
	if err := os.WriteFile(fakeCodex, []byte("#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -- ]; then shift; exec \"$@\"; fi; shift; done\nexit 8\n"), 0700); err != nil {
		t.Fatal(err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("fixture requires node:", err)
	}
	cfg := Configuration{Host: "fixture", Fleet: Fleet{Node: node, GiteaURL: f.server.URL, RuntimeDir: runtimeDir, Git: "/usr/bin/git", GitUser: "Fixture", Codex: fakeCodex, Claude: "/fixture/claude", WorkerTokenFile: filepath.Join(f.root, "worker-token"), Model: "model", ClaudeModel: "opus", JobTimeoutSeconds: 30, TestTimeoutSeconds: fixtureTestTimeoutSeconds, Repositories: []Repository{{Name: "owner/project", AutoMerge: true, TestCommand: []string{node, "--test", "feature.test.js"}}}}}
	cfg.Assignments.WorkerAccounts = map[string]string{"fixture": "worker"}
	cfg.Assignments.ImplementationRunLimit = 4
	cfg.Assignments.ImplementationUsageLimit = 8000000
	cfg.ReleasePolicy = &ReleasePolicy{PermissionFiles: []string{}}
	if mode == "permission" {
		cfg.ReleasePolicy.PermissionFiles = []string{"protected.json"}
	}
	snapshot := intake.ConfigSnapshot{Version: "v", ConfigurationVersion: "c", SettingsVersion: "s", Config: encode(cfg.Fleet), Settings: json.RawMessage(`{}`)}
	intakeServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/client-config":
			if r.URL.Query().Has("since") {
				t.Error("設定取得はsinceを指定せず完全な応答を要求する")
			}
			copy := snapshot
			copy.ReviewNotes = f.CurrentReviewNotes
			if f.CurrentPolicy != nil {
				copy.Config = encode(f.CurrentPolicy.Fleet)
				json.NewEncoder(w).Encode(copy)
			} else {
				json.NewEncoder(w).Encode(copy)
			}
		case r.URL.Path == "/api/v1/jobs/1":
			var descriptor map[string]any
			_ = json.Unmarshal(f.Job.Raw, &descriptor)
			descriptor["status"] = "running"
			descriptor["leaseHost"] = "fixture"
			descriptor["leaseEpoch"] = 3
			if f.CurrentLeaseEpoch > 0 {
				descriptor["leaseEpoch"] = f.CurrentLeaseEpoch
			}
			descriptor["pr"] = 1
			descriptor["headSha"] = f.head
			if f.CurrentJobHead != "" {
				descriptor["headSha"] = f.CurrentJobHead
			}
			descriptor["view"] = f.CurrentView
			if f.CurrentBranch != "" {
				descriptor["branch"] = f.CurrentBranch
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"job": descriptor})
		case r.URL.Path == "/api/v1/usage":
			var batch struct {
				Usage  []json.RawMessage
				Limits []json.RawMessage
			}
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
				t.Error(err)
			}
			_ = json.NewEncoder(w).Encode(map[string]int{"added": len(batch.Usage) + len(batch.Limits), "duplicate": 0, "rejected": 0})
		case r.URL.Path == "/api/v1/jobs/1/scope":
			var body struct {
				Epoch  int64    `json:"epoch"`
				Add    []string `json:"add"`
				Actual []string `json:"actual"`
			}
			epoch := int64(3)
			if f.CurrentLeaseEpoch > 0 {
				epoch = f.CurrentLeaseEpoch
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Epoch != epoch {
				t.Error("範囲申請のepochが不正です")
			}
			if body.Actual != nil {
				if f.ScopeActual != nil {
					f.ScopeActual(w, body.Actual)
					break
				}
				json.NewEncoder(w).Encode(map[string]any{"result": "granted", "job": map[string]any{"id": 1, "scope": body.Actual}})
				break
			}
			f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch)
			if f.ScopeAdd != nil {
				f.ScopeAdd(w, body.Add)
				break
			}
			if len(body.Add) == 1 && body.Add[0] == "revision.txt" {
				fmt.Fprint(w, `{"result":"granted","job":{"id":1,"scope":["feature.txt","revision.txt"]}}`)
				break
			}
			if len(body.Add) != 1 || body.Add[0] != "feature.txt" {
				t.Error("実差分/現在epochと異なる範囲を申請しました")
			}
			outcome := "granted"
			if f.Mode == "scope-wait" {
				outcome = "wait"
			}
			fmt.Fprintf(w, `{"result":%q,"job":{"id":1,"scope":["README.md","feature.txt"]}}`, outcome)
		case r.URL.Path == "/api/v1/jobs/1/notes":
			var note map[string]any
			if err := json.NewDecoder(r.Body).Decode(&note); err != nil {
				t.Error(err)
			}
			f.Notes = append(f.Notes, note)
			io.WriteString(w, `{"id":1}`)
		case r.URL.Path == "/api/v1/jobs/1/state":
			if f.StateStatus != 0 {
				w.WriteHeader(f.StateStatus)
				io.WriteString(w, `{"title":"拒否","code":"request_failed"}`)
				return
			}
			var body map[string]any
			raw, readErr := io.ReadAll(r.Body)
			epoch := int64(3)
			if f.CurrentLeaseEpoch > 0 {
				epoch = f.CurrentLeaseEpoch
			}
			if readErr != nil || json.Unmarshal(raw, &body) != nil || body["epoch"] != float64(epoch) {
				t.Error("checkpointのepochが不正です")
			}
			var received intake.State
			_ = json.Unmarshal(raw, &received)
			f.ReceivedStates = append(f.ReceivedStates, received)
			if head, ok := body["headSha"].(string); ok && head != "" {
				f.CurrentJobHead = head
			}
			io.WriteString(w, `{"job":{"id":1}}`)
		case r.URL.Path == "/api/v1/requests/next":
			f.mu.Lock()
			defer f.mu.Unlock()
			f.RequestKinds = append(f.RequestKinds, r.URL.Query().Get("kind"))
			if f.NextRequest != nil && f.NextRequest.Kind == r.URL.Query().Get("kind") {
				json.NewEncoder(w).Encode(map[string]any{"request": f.NextRequest})
			} else {
				io.WriteString(w, `{"request":null}`)
			}
		case strings.HasPrefix(r.URL.Path, "/api/v1/requests/") && (strings.HasSuffix(r.URL.Path, "/result") || strings.HasSuffix(r.URL.Path, "/release")):
			f.mu.Lock()
			defer f.mu.Unlock()
			f.RequestPosts = append(f.RequestPosts, r.URL.Path)
			io.WriteString(w, `{}`)
		case strings.HasPrefix(r.URL.Path, "/api/v1/commands/") && strings.HasSuffix(r.URL.Path, "/ack"):
			if f.AckStatus != 0 {
				w.WriteHeader(f.AckStatus)
			}
			io.WriteString(w, `{}`)
		case r.URL.Path == "/api/v1/commands":
			json.NewEncoder(w).Encode(map[string]any{"commands": f.Commands, "next": 0})
		case strings.HasPrefix(r.URL.Path, "/api/v1/merge-turns/"):
			f.TurnCalls = append(f.TurnCalls, r.URL.Path)
			if f.TurnResponse != nil && f.TurnResponse(w, r) {
				return
			}
			if strings.HasSuffix(r.URL.Path, "/renew") && f.TurnRenewStatus != 0 {
				w.WriteHeader(f.TurnRenewStatus)
				io.WriteString(w, `{"code":"turn_lost"}`)
				return
			}
			if strings.HasSuffix(r.URL.Path, "/release") {
				io.WriteString(w, `{}`)
			} else {
				fmt.Fprintf(w, `{"granted":true,"turn":{"epoch":2,"expiresAt":%q}}`, time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
			}
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/claude-api-key"):
			io.WriteString(w, `{"state":"unregistered","version":null,"key":null}`)
		case f.AllowUsage && r.URL.Path == "/api/v1/jobs/1/events":
			json.NewEncoder(w).Encode(map[string]any{"job": map[string]int{"id": 1}, "usageLimitReason": f.UsageLimitReason})
		default:
			t.Errorf("予期しないIntake経路: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(intakeServer.Close)
	client, err := intake.New(configBootstrap(intakeServer.URL), "idk_"+strings.Repeat("a", 43), intakeServer.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	f.API = client
	g, err := gitea.New(f.server.URL, strings.Repeat("b", 40))
	if err != nil {
		t.Fatal(err)
	}
	f.Executor = &Executor{Config: cfg, API: client, Gitea: g, Store: store, Executable: os.Args[0], ready: true}
	f.Executor.Call = func(ctx context.Context, request providers.Request) (providers.Answer, error) {
		f.Calls = append(f.Calls, request.Settings.Provider)
		zero, one := int64(0), int64(1)
		answer := providers.Answer{Usage: providers.Usage{Input: &one, Output: &one, Cached: &zero, CacheWrite: &zero}, Outcome: "completed"}
		if !request.Review {
			if err := os.WriteFile(filepath.Join(request.Policy.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if mode == "permission" {
				os.WriteFile(filepath.Join(request.Policy.Tree, "protected.json"), []byte("{}\n"), 0600)
			}
			if mode == "sql" || mode == "sql-delete" {
				sql := "DROP TABLE production_data;\n"
				if mode == "sql-delete" {
					sql = "DELETE FROM production_data;\n"
				}
				os.WriteFile(filepath.Join(request.Policy.Tree, "schema.sql"), []byte(sql), 0600)
			}
			answer.JSON = implementationFixture()
		} else {
			if !request.Policy.ReadOnly {
				t.Error("レビューが読み取り専用ではありません")
			}
			decision, breaking := "approve", false
			if mode == "review-hold" {
				decision = "hold"
				breaking = true
			}
			answer.JSON = encode(map[string]any{"decision": decision, "summary": "検査結果", "breaking": breaking, "tooLarge": false, "findings": []string{}, "findingKinds": []string{}, "findingSeverities": []string{}, "findingSpecs": []any{}, "followups": []string{}, "followupSpecs": []any{}, "improvements": []string{}, "postMergeChecks": []string{}, "userJudgments": []string{}, "resolvableUncertainties": []string{}})
		}
		return answer, nil
	}
	descriptor := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Repo: "owner/project", Issue: 7, Title: "Implement feature", Provider: "codex", Scope: []string{"*"}, View: map[string]json.RawMessage{}}}
	if mode == "scope-granted" || mode == "scope-wait" {
		descriptor.Scope = []string{"README.md"}
	}
	f.Job = intake.Job{ID: 1, Repo: "owner/project", LeaseEpoch: 3, Raw: encode(descriptor)}
	return f
}
func implementationFixture() json.RawMessage {
	return encode(map[string]any{"status": "completed", "summary": "機能を実装", "incomplete": nil, "prerequisiteMissing": nil, "questions": []string{}, "tests": []any{}, "unverified": []string{}, "unverifiedKinds": []string{}, "followupIssues": []any{}, "acceptanceTransfers": []any{}, "prevention": map[string]string{"kind": "test", "detail": "生成ファイルを固定コマンドが検査"}, "assumptions": []any{map[string]any{"assumption": "作業対象はfixture", "evidence": "割当て記述子", "verified": true}}, "similar": map[string]any{"searched": "対象パスを照合", "found": []string{}, "handled": "対象ファイルへ実装", "guard": "固定テストで検査"}, "selfCheck": []any{map[string]any{"criterion": "feature.txtを生成", "source": "issue", "location": "feature.txt:1", "test": "固定テストが実ファイルを検査", "production": map[string]string{"origin": "Issue", "stored": "feature.txt", "caller": "固定テスト", "outlet": "PR"}, "similarLocations": []any{map[string]string{"location": "fixture", "result": "対象経路を検査"}}, "testChanges": map[string]any{"changed": false, "details": "設定済み固定コマンドが検証"}}}})
}
func TestPipelineCommitsTestsReviewsPushesAndFastForwardMerges(t *testing.T) {
	f := newPipeline(t, "approve")
	fences := 0
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { fences++; return nil })
	if err != nil || state.Status != "merged" {
		t.Fatal(state, err)
	}
	if !f.merged || fences < 2 {
		t.Fatal("所有確認・実マージを通っていません")
	}
	if strings.Join(f.Calls, ",") != "codex,claude" {
		t.Fatal("実装と独立レビューが反対側ではありません", f.Calls)
	}
	if f.git(f.root, "--git-dir", f.bare, "show", "main:feature.txt") != "accepted feature" {
		t.Fatal("生成したファイルが既定ブランチへ入りませんでした")
	}
}
func TestLeaseLossBeforePushDoesNotPublish(t *testing.T) {
	f := newPipeline(t, "approve")
	_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return &intake.APIError{Status: 409, Code: "lease_lost"} })
	var lost *intake.APIError
	if !errors.As(err, &lost) || !lost.LeaseLost() || f.pull || f.merged {
		t.Fatal("lease喪失後に公開しました", err)
	}
}
func TestProductionSQLWaitsForDeletionApproval(t *testing.T) {
	f := newPipeline(t, "sql")
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || state.HoldReason != "deletions" || f.merged {
		t.Fatal(state, err)
	}
}
func TestPermissionChangeWaitsForBoundApproval(t *testing.T) {
	f := newPipeline(t, "permission")
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || f.merged {
		t.Fatal(state, err)
	}
	var view map[string]json.RawMessage
	json.Unmarshal(state.View, &view)
	if len(view["permissionHold"]) == 0 {
		t.Fatal("対象SHAと権限対象を報告していません")
	}
}

func configBootstrap(url string) config.Bootstrap {
	return config.Bootstrap{ServerURL: url, ServerTokenFile: "unused", WorkerID: "fixture"}
}

func TestScopeMetadataIsUpdatedOnlyAfterBranchIsPushed(t *testing.T) {
	f := newPipeline(t, "scope-granted")
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || !f.merged {
		t.Fatal(state, err)
	}
}
func TestOverlappingScopeHandsBackWithoutFinalLeaseOrState(t *testing.T) {
	f := newPipeline(t, "scope-wait")
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if !errors.Is(err, intake.ErrJobReleased) || state.Status != "held" || state.HoldReason != "scope_wait" || f.merged || f.pull {
		t.Fatal(state, err)
	}
	if !strings.Contains(string(state.Result), "scope_wait") || !strings.Contains(string(state.Result), "duration_ms") {
		t.Fatalf("scope release diagnostic missing from saved summary: %s", state.Result)
	}
	record, loadErr := f.Executor.Store.Load(1)
	if loadErr != nil || !record.FinalReported {
		t.Fatal("Serverが保存済みの保留を再報告しようとしました", loadErr)
	}
}

func TestReviewClassificationExecutionPaths(t *testing.T) {
	parallelCases := make(chan struct{}, 4)
	for _, tc := range []struct {
		name                                                       string
		judgment, uncertainty, resolvable, finding, kind, severity string
		breaking                                                   bool
		want                                                       string
		reviews                                                    int
	}{
		{name: "user", judgment: "利用者の選択", want: "held", reviews: 1},
		{name: "breaking", breaking: true, want: "held", reviews: 1},
		{name: "legacy uncertainty", uncertainty: "旧判断", want: "held", reviews: 1},
		{name: "resolvable", resolvable: "追加確認", want: "merged", reviews: 1},
		{name: "must and resolvable", resolvable: "追加確認", finding: "修正", kind: "other", severity: "must", want: "held", reviews: 2},
		{name: "regression minor", resolvable: "追加確認", finding: "回帰", kind: "regression", severity: "minor", want: "merged", reviews: 1},
		{name: "security minor", resolvable: "追加確認", finding: "安全性", kind: "security", severity: "minor", want: "merged", reviews: 2},
		{name: "data minor", resolvable: "追加確認", finding: "データ", kind: "data", severity: "minor", want: "merged", reviews: 2},
		{name: "blocking minor", resolvable: "追加確認", finding: "停止", kind: "blocking", severity: "minor", want: "merged", reviews: 2},
		{name: "no findings", want: "merged", reviews: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parallelCases <- struct{}{}
			defer func() { <-parallelCases }()
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			implementations := 0
			original := f.Executor.Call
			reviews := 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := original(ctx, req)
				if err != nil {
					return a, err
				}
				if !req.Review {
					implementations++
					if implementations > 1 {
						if !strings.Contains(req.Prompt, tc.finding) || !strings.Contains(req.Prompt, tc.resolvable) {
							t.Error("差し戻しに両方の内容がありません")
						}
						if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte("revised feature\n"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return a, err
				}
				reviews++
				if tc.severity == "minor" && tc.kind != "regression" && reviews > 1 {
					return a, nil
				}
				var v map[string]any
				_ = json.Unmarshal(a.JSON, &v)
				v["breaking"] = tc.breaking
				if tc.judgment != "" {
					v["userJudgments"] = []string{tc.judgment}
				}
				if tc.uncertainty != "" {
					v["uncertainties"] = []string{tc.uncertainty}
				}
				if tc.resolvable != "" {
					v["resolvableUncertainties"] = []string{tc.resolvable}
					v["decision"] = "hold"
				}
				if tc.finding != "" {
					v["findings"] = []string{tc.finding}
					v["findingKinds"] = []string{tc.kind}
					v["findingSeverities"] = []string{tc.severity}
					v["decision"] = "hold"
				}
				a.JSON = encode(v)
				return a, nil
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != tc.want || reviews != tc.reviews {
				t.Fatalf("state=%+v err=%v reviews=%d", state, err, reviews)
			}
			if tc.judgment != "" && !strings.Contains(string(state.Result), "利用者の判断: 利用者の選択") {
				t.Fatal(string(state.Result))
			}
			if tc.finding != "" && tc.want == "held" && (!strings.Contains(string(state.Result), tc.finding) || !strings.Contains(string(state.Result), tc.resolvable)) {
				t.Fatal(string(state.Result))
			}
			if tc.name == "resolvable" && !strings.Contains(string(state.Result), `"improvements":["追加確認"]`) {
				t.Fatal(string(state.Result))
			}
		})
	}
}

func TestReviewNotesReworkThenMerge(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	original := f.Executor.Call
	reviews, implementations := 0, 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, req)
		if err != nil {
			return answer, err
		}
		if !req.Review {
			implementations++
			if implementations > 1 {
				if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte("revised\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			return answer, nil
		}
		reviews++
		var v map[string]any
		_ = json.Unmarshal(answer.JSON, &v)
		if reviews == 1 {
			v["decision"] = "hold"
			v["findings"] = []string{"must fix"}
			v["findingKinds"] = []string{"security"}
			v["findingSeverities"] = []string{"must"}
		} else {
			v["followups"] = []string{"follow"}
			v["improvements"] = []string{"improve"}
		}
		answer.JSON = encode(v)
		return answer, nil
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || len(f.Notes) != 3 {
		t.Fatalf("state=%s err=%v notes=%v", state.Status, err, f.Notes)
	}
	for i, kind := range []string{"finding", "followup", "improvement"} {
		if f.Notes[i]["kind"] != kind {
			t.Fatal(f.Notes)
		}
	}
}

func TestDifferentHeavyReviewsContinueUntilUsageLimit(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	original := f.Executor.Call
	reviews, implementations := 0, 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		a, err := original(ctx, req)
		if err != nil {
			return a, err
		}
		if !req.Review {
			implementations++
			if implementations > 1 {
				if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte(fmt.Sprint(implementations)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			return a, nil
		}
		reviews++
		var v map[string]any
		if err := json.Unmarshal(a.JSON, &v); err != nil {
			t.Fatal(err)
		}
		v["decision"] = "hold"
		v["findings"] = []string{fmt.Sprintf("異なる重大な指摘 %d", reviews)}
		v["findingKinds"] = []string{"regression"}
		v["findingSeverities"] = []string{"must"}
		a.JSON = encode(v)
		if reviews == 3 {
			reason := "使用量上限"
			f.UsageLimitReason = &reason
		}
		return a, nil
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || state.HoldReason != "question" || reviews != 3 || implementations != 3 || !strings.Contains(string(state.Result), "使用量上限") {
		t.Fatalf("state=%+v err=%v reviews=%d implementations=%d", state, err, reviews, implementations)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil || saved.ReviewHolds != 3 {
		t.Fatalf("saved=%+v err=%v", saved, err)
	}
	var view map[string]any
	if err := json.Unmarshal(state.View, &view); err != nil || view["reviewHolds"] != float64(3) || view["implementationUsageStop"] != true {
		t.Fatalf("view=%v err=%v", view, err)
	}
}

func fixtureShellCommand(f *pipelineFixture, command string) []string {
	return []string{f.Executor.Config.Node, "-e", "require(process.argv[1])", "./fixture-command.test.js", command}
}

func TestInitialFixedTestFollowsImplementationBase(t *testing.T) {
	f := newPipeline(t, "approve")
	log := filepath.Join(f.root, "tested-heads")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, fmt.Sprintf("git rev-parse HEAD >> %q && test -f upstream.txt", log))
	original := f.Executor.Call
	var base string
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, req)
		if !req.Review && err == nil {
			source := filepath.Join(f.root, "source")
			if err := os.WriteFile(filepath.Join(source, "upstream.txt"), []byte("upstream\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(source, "add", "upstream.txt")
			f.git(source, "commit", "-m", "advance base")
			f.git(source, "push", f.bare, "main")
			base = f.git(source, "rev-parse", "HEAD")
		}
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || strings.Join(f.Calls, ",") != "codex,claude" {
		t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	heads := strings.Fields(string(data))
	if len(heads) != 1 {
		t.Fatalf("tested heads: %v", heads)
	}
	f.git(filepath.Join(f.root, "source"), "fetch", f.bare, f.Branch)
	f.git(filepath.Join(f.root, "source"), "merge-base", "--is-ancestor", base, heads[0])
}

func TestFixedTestRechecksBaseAdvancedAfterStart(t *testing.T) {
	f := newPipeline(t, "approve")
	log := filepath.Join(f.root, "tested-heads")
	source := filepath.Join(f.root, "source")
	command := fmt.Sprintf("git rev-parse HEAD >> %q; if [ ! -f %q ]; then touch %q; git -C %q add upstream.txt && git -C %q commit -m advance && git -C %q push %q main; fi", log, filepath.Join(f.root, "advanced"), filepath.Join(f.root, "advanced"), source, source, source, f.bare)
	if err := os.WriteFile(filepath.Join(source, "upstream.txt"), []byte("upstream\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, command)
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || strings.Join(f.Calls, ",") != "codex,claude" {
		t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	heads := strings.Fields(string(data))
	if len(heads) != 2 || heads[0] == heads[1] {
		t.Fatalf("tested heads: %v", heads)
	}
	base := f.git(source, "rev-parse", "main")
	f.git(source, "fetch", f.bare, f.Branch)
	f.git(source, "merge-base", "--is-ancestor", base, heads[1])
}

// Prepare evidence for fixtures that begin at a later execution stage.
func fixtureRecordEvidence(t *testing.T, f *pipelineFixture, r *journal.Record, tree string) {
	t.Helper()
	r.Tree = tree
	result, err := f.Executor.fixedTest(context.Background(), f.Executor.Config.Repositories[0], r)
	if err != nil || !result.Passed || testEvidenceReason(result, r.Head, r.Base) != "" {
		t.Fatalf("fixture test evidence: %+v: %v", result, err)
	}
	r.Test = encode(result)
}

// alwaysHoldingReview makes every independent review send the change back with a
// new must finding, so only the run and usage limits can end the execution.
func alwaysHoldingReview(t *testing.T, f *pipelineFixture, reviews, implementations *int, afterReview func(int)) {
	t.Helper()
	original := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		a, err := original(ctx, req)
		if err != nil {
			return a, err
		}
		if !req.Review {
			*implementations++
			if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte(fmt.Sprint(*implementations)), 0600); err != nil {
				t.Fatal(err)
			}
			return a, nil
		}
		*reviews++
		var v map[string]any
		if err := json.Unmarshal(a.JSON, &v); err != nil {
			t.Fatal(err)
		}
		v["decision"] = "hold"
		v["findings"] = []string{fmt.Sprintf("異なる重大な指摘 %d", *reviews)}
		v["findingKinds"] = []string{"regression"}
		v["findingSeverities"] = []string{"must"}
		a.JSON = encode(v)
		if afterReview != nil {
			afterReview(*reviews)
		}
		return a, nil
	}
}

func stateWorkerExecution(t *testing.T, state intake.State) map[string]any {
	t.Helper()
	var view struct {
		Execution map[string]any `json:"workerExecution"`
	}
	if err := json.Unmarshal(state.View, &view); err != nil {
		t.Fatal(err)
	}
	return view.Execution
}

// 差し戻しからの再実装は世代を進めないが、同じ実行の中でも実装回数に数えて上限で止める。
// 数えた回数は journal に残り、上限超えの承認の後の再開でも引き継ぐ。
func TestReviewReworkStopsAtRunLimit(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	f.Executor.Config.Assignments.ImplementationRunLimit = 4
	tr := completionAPI(t, f)
	reviews, implementations := 0, 0
	alwaysHoldingReview(t, f, &reviews, &implementations, nil)
	state, err := execute(f)
	if err != nil || state.Status != "held" || state.HoldReason != "question" || !strings.Contains(string(state.Result), "実装回数の上限に達しました") || !strings.Contains(string(state.View), `"implementationRunLimit":true`) {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if implementations != 4 || reviews != 4 {
		t.Fatalf("implementations=%d reviews=%d", implementations, reviews)
	}
	execution := stateWorkerExecution(t, state)
	if execution["implementationRuns"] != float64(5) || execution["reviewReworkRuns"] != float64(3) {
		t.Fatalf("workerExecution=%v", execution)
	}
	// 初回は利用者が始めた世代なので検査しない。差し戻しからの3回の再実装は、毎回開始の前に使用量を確かめる。
	if len(tr.checks) != 3 {
		t.Fatalf("usage checks=%d", len(tr.checks))
	}

	acknowledgeFinal(t, f)
	setGeneration(t, f, 0, map[string]any{"overLimitApprovals": []map[string]any{{"id": "a", "statement": "承認", "implementationRuns": 5}}, "instructions": []map[string]string{{"id": "human:approval", "text": "承認後に検討を続ける"}}})
	state, err = execute(f)
	if err != nil || !strings.Contains(string(state.View), `"implementationRunLimit":true`) {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	// 承認で上限が6になり、再開後の差し戻しは保存済みの3回の続きから数える。
	if implementations != 6 {
		t.Fatalf("implementations after approval=%d", implementations)
	}
	if execution := stateWorkerExecution(t, state); execution["implementationRuns"] != float64(7) {
		t.Fatalf("workerExecution=%v", execution)
	}
}

// 使用量の承認を受けた世代でも、差し戻しからの再実装は承認を使い回さず使用量の上限で止める。
func TestReviewReworkAfterAdmittedStartStopsAtUsageLimit(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	setGeneration(t, f, 0, map[string]any{"reimplementExpected": map[string]any{"generation": 0, "usageAdmission": true}})
	tr := completionAPI(t, f)
	reviews, implementations := 0, 0
	alwaysHoldingReview(t, f, &reviews, &implementations, func(int) { tr.reason = "使用量上限" })
	state, err := execute(f)
	if err != nil || state.Status != "held" || state.HoldReason != "question" || !strings.Contains(string(state.Result), "使用量上限") || !strings.Contains(string(state.View), `"implementationUsageStop":true`) {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	if implementations != 1 || reviews != 1 || len(tr.checks) != 2 {
		t.Fatalf("implementations=%d reviews=%d checks=%d", implementations, reviews, len(tr.checks))
	}
	if !strings.Contains(string(tr.checks[0]), `"admissionId"`) || strings.Contains(string(tr.checks[1]), `"admissionId"`) {
		t.Fatalf("checks=%s", tr.checks)
	}
}

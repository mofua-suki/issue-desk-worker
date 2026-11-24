package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
)

type nativeTransport func(*http.Request) (*http.Response, error)

func (f nativeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func nativeResponse(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b)))}
}
func nativeGitFixture(t *testing.T) (*gitwork.Workspace, string, string, string) {
	t.Helper()
	root := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("config", "user.name", "test")
	run("config", "user.email", "test@localhost")
	run("commit", "--allow-empty", "-m", "base")
	base := run("rev-parse", "HEAD")
	run("update-ref", "refs/remotes/origin/main", base)
	if err = os.WriteFile(filepath.Join(root, "feature"), []byte("feature"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "feature")
	run("commit", "-m", "feature")
	head := run("rev-parse", "HEAD")
	tree := run("rev-parse", "HEAD^{tree}")
	// Remote fetch is an external boundary; the fixture keeps the remote tracking
	// reference local while all merge-tree computation uses real Git objects.
	wrapper := filepath.Join(root, "git-fixture")
	if err = os.WriteFile(wrapper, []byte("#!/bin/sh\nfor arg do if [ \"$arg\" = fetch ]; then exit 0; fi; done\nexec "+shellArg(git)+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return &gitwork.Workspace{Config: gitwork.Config{Git: wrapper, Root: root, User: "test"}, Tree: root, Mirror: filepath.Join(root, ".git"), Default: "main", Repo: "owner/project"}, head, base, tree
}
func shellArg(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }

func TestNativeGateRequiresPublishedExactPassingResult(t *testing.T) {
	failureCases := map[string]struct{ message, reason string }{
		"failure-deps":         {"Flutterの依存関係を取得できません。", "Flutterの依存関係"},
		"failure-lint-package": {"独自Lintパッケージの検証に失敗しました。", "独自Lintパッケージ"},
		"failure-analysis":     {"Flutterの解析に失敗しました。", "Flutterの解析"},
		"failure-app-lint":     {"アプリの独自Lintに失敗しました。", "アプリの独自Lint"},
		"failure-test":         {"Flutterのテストに失敗しました。", "Flutterのテスト"},
		"failure-build":        {"iPhoneシミュレーター向けのビルドに失敗しました。", "iPhoneシミュレーター向けのビルド"},
		"failure-unknown":      {"未知の失敗", ""},
		"failure-not-suffix":   {"Flutterのテストに失敗しました。 trailing", ""},
	}
	scenarios := []string{"passed", "head-mismatch", "base-mismatch", "tree-mismatch", "multiple-mismatch", "failed", "unrunnable", "interrupted", "unknown", "expired", "missing-result", "unpublished", "policy-change", "lease-lost", "ambiguous-submit", "already-created"}
	for name := range failureCases {
		scenarios = append(scenarios, name)
	}
	for _, scenario := range scenarios {
		t.Run(scenario, func(t *testing.T) {
			w, head, base, tree := nativeGitFixture(t)
			c := Configuration{Host: "linux", Version: "c", Fleet: Fleet{Repositories: []Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"test"}}}}}
			c.Assignments.Hosts = json.RawMessage(`["linux","apple"]`)
			c.Assignments.HostRoles = map[string]string{"apple": "test-only"}
			c.Assignments.TestRepositories = map[string][]string{"apple": {"owner/project"}}
			target := nativeTarget{Host: "apple", Head: head, Base: base, Tree: tree}
			result := nativeResult{Status: "passed", Head: head, Base: base, Tree: tree, Message: "private runner message"}
			switch scenario {
			case "head-mismatch":
				result.Head = base
			case "base-mismatch":
				result.Base = head
			case "tree-mismatch":
				result.Tree = head
			case "multiple-mismatch":
				result.Head, result.Base, result.Tree = base, head, head
			case "failed", "unrunnable", "interrupted", "unknown":
				result.Status = scenario
			}
			if failure, ok := failureCases[scenario]; ok {
				result.Status = "failed"
				result.Message = "private body /secret/path token=credential: " + failure.message + "\n"
			}
			created, cancelled, reads := 0, 0, 0
			id := nativeID("owner/project", target, c.Version, c.Host)
			transport := nativeTransport(func(r *http.Request) (*http.Response, error) {
				switch {
				case r.URL.Path == "/api/v1/client-config":
					version := "c"
					if scenario == "policy-change" {
						version = "changed"
					}
					return nativeResponse(200, intake.ConfigSnapshot{Version: "v", ConfigurationVersion: version, SettingsVersion: "s", Config: encode(c.Fleet)}), nil
				case strings.HasSuffix(r.URL.Path, "/pulls/1"):
					p := gitea.Pull{Number: 1, State: "open"}
					p.Head.SHA = head
					p.Base.SHA = base
					if scenario == "unpublished" {
						p.Head.SHA = base
					}
					return nativeResponse(200, p), nil
				case r.URL.Path == "/api/v1/requests" && r.Method == "POST":
					created++
					var request monitorRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Fatal(err)
					}
					var got nativeTarget
					_ = json.Unmarshal(request.Payload, &got)
					if request.ID != id || got != target || request.Kind != "native_test" || request.Repo != "owner/project" {
						t.Fatalf("wrong request: %+v", request)
					}
					if scenario == "ambiguous-submit" {
						return nil, errors.New("lost response")
					}
					return nativeResponse(200, map[string]any{}), nil
				case strings.HasSuffix(r.URL.Path, "/cancel"):
					cancelled++
					return nativeResponse(200, map[string]any{}), nil
				case r.URL.Path == "/api/v1/requests/"+id:
					reads++
					if reads == 1 && scenario != "already-created" {
						return nativeResponse(404, map[string]any{"code": "not_found"}), nil
					}
					req := nativeRequestState{Request: monitorRequest{ID: id, Kind: "native_test", Repo: "owner/project", Payload: encode(target)}, Status: "completed", Result: &result}
					if scenario == "expired" {
						req.Status = "expired"
					}
					if scenario == "missing-result" {
						req.Result = nil
					}
					return nativeResponse(200, map[string]any{"request": req}), nil
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
					return nil, errors.New("unexpected")
				}
			})
			old := http.DefaultTransport
			http.DefaultTransport = transport
			defer func() { http.DefaultTransport = old }()
			api, err := intake.New(config.Bootstrap{ServerURL: "https://intake.invalid", WorkerID: "linux", ServerTokenFile: "unused"}, "idk_"+strings.Repeat("a", 43), transport)
			if err != nil {
				t.Fatal(err)
			}
			g, err := gitea.New("https://git.invalid", "token")
			if err != nil {
				t.Fatal(err)
			}
			e := &Executor{Config: c, API: api, Gitea: g}
			err = e.nativeGate(context.Background(), "owner/project", 1, w, head, base, func(context.Context) error {
				if scenario == "lease-lost" {
					return errors.New("lease lost")
				}
				return nil
			})
			success := scenario == "passed" || scenario == "already-created"
			if (err == nil) != success {
				t.Fatalf("success=%v err=%v", success, err)
			}
			if !success {
				var hold *releaseHold
				if !errors.As(err, &hold) {
					t.Fatalf("not a publication hold: %v", err)
				}
			}
			wantReasons := map[string]string{
				"head-mismatch":     "native testのheadが一致しません",
				"base-mismatch":     "native testのbaseが一致しません",
				"tree-mismatch":     "native testのtreeが一致しません",
				"multiple-mismatch": "native testのhead/base/treeが一致しません",
				"failed":            "native testが不合格です",
				"unrunnable":        "native testを実行できませんでした",
				"interrupted":       "native testが中断されました",
				"unknown":           "native testの結果が不明です",
				"missing-result":    "native testが結果なしで終了しました",
			}
			if failure, ok := failureCases[scenario]; ok {
				wantReasons[scenario] = "native testが不合格です"
				if failure.reason != "" {
					wantReasons[scenario] += "（" + failure.reason + "）"
				}
			}
			if reason, ok := wantReasons[scenario]; ok {
				want := "Appleネイティブ検証を確認できないためマージを保留します: " + reason
				if err == nil || err.Error() != want {
					t.Fatalf("hold reason = %v, want %q", err, want)
				}
			}
			if created > 1 {
				t.Fatal("submission retried")
			}
			if scenario == "already-created" && created != 0 {
				t.Fatal("existing request resubmitted")
			}
			if scenario == "ambiguous-submit" && cancelled != 1 {
				t.Fatal("ambiguous request not cancelled")
			}
		})
	}
}
func TestNativeUnsetAndTestOnlyAdmission(t *testing.T) {
	c := Configuration{Host: "apple", Fleet: Fleet{Repositories: []Repository{{Name: "owner/project", TestCommand: []string{"true"}}}}}
	e := &Executor{Config: c}
	if err := e.nativeGate(context.Background(), "owner/project", 1, nil, "", "", nil); err != nil {
		t.Fatal(err)
	}
	c.Assignments.Hosts = json.RawMessage(`["apple"]`)
	c.Assignments.HostRoles = map[string]string{"apple": "test-only"}
	c.Assignments.Implementers = map[string][]string{"owner/project": {"apple"}}
	e.Config = c
	if len(c.RepoLimits()) != 0 || len(e.requestRepos()) != 0 {
		t.Fatal("test-only admitted implementation/PR work")
	}
	if _, err := New(context.Background(), c, nil); err == nil {
		t.Fatal("implementation runner admitted test-only")
	}
	if err := Check(context.Background(), c, ""); err == nil {
		t.Fatal("implementation check admitted test-only")
	}
}

func TestNativeGateProtectsIssueAndManualMerge(t *testing.T) {
	for _, entry := range []string{"issue", "manual"} {
		for _, outcome := range []string{"passed", "mismatch", "failed", "unset"} {
			t.Run(entry+"/"+outcome, func(t *testing.T) {
				f := newPipeline(t, "approve")
				if outcome != "unset" {
					f.Executor.Config.Repositories[0].NativeTestHost = "apple"
				}
				f.Executor.Config.Assignments.Hosts = json.RawMessage(`["linux","apple"]`)
				f.Executor.Config.Assignments.HostRoles = map[string]string{"apple": "test-only"}
				f.Executor.Config.Assignments.TestRepositories = map[string][]string{"apple": {"owner/project"}}
				policy := f.Executor.Config
				f.CurrentPolicy = &policy
				original := f.API
				var request *nativeRequestState
				created := 0
				transport := nativeTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/v1/requests" && r.Method == "POST" {
						var sent monitorRequest
						if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
							t.Fatal(err)
						}
						var target nativeTarget
						_ = json.Unmarshal(sent.Payload, &target)
						// The head must already be present in the published remote branch.
						if target.Head != f.head {
							t.Fatal("tested unpublished head")
						}
						res := nativeResult{Status: "passed", Head: target.Head, Base: target.Base, Tree: target.Tree, Message: "ok"}
						if outcome == "mismatch" {
							res.Tree = target.Base
						}
						if outcome == "failed" {
							res.Status = "failed"
						}
						request = &nativeRequestState{Request: sent, Status: "completed", Result: &res}
						created++
						return nativeResponse(200, map[string]any{}), nil
					}
					if strings.HasPrefix(r.URL.Path, "/api/v1/requests/") && r.Method == "GET" {
						if request == nil {
							return nativeResponse(404, map[string]any{"code": "not_found"}), nil
						}
						return nativeResponse(200, map[string]any{"request": request}), nil
					}
					if strings.HasSuffix(r.URL.Path, "/cancel") {
						return nativeResponse(200, map[string]any{}), nil
					}
					var body json.RawMessage
					if r.Body != nil {
						body, _ = io.ReadAll(r.Body)
					}
					res, err := original.Explicit(r.Context(), r.Method, r.URL.RequestURI(), body)
					if err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: res.Status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(res.Body)))}, nil
				})
				api, err := intake.New(config.Bootstrap{ServerURL: "https://fixture.invalid", WorkerID: "fixture", ServerTokenFile: "unused"}, "idk_"+strings.Repeat("a", 43), transport)
				if err != nil {
					t.Fatal(err)
				}
				f.Executor.API = api
				wantMerge := outcome == "passed" || outcome == "unset"
				if entry == "issue" {
					state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
					if err != nil {
						t.Fatal(err)
					}
					if (state.Status == "merged") != wantMerge {
						t.Fatalf("state: %+v", state)
					}
				} else {
					source := filepath.Join(f.root, "source")
					f.Branch = "chat/native"
					f.git(source, "checkout", "-b", f.Branch)
					if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
						t.Fatal(err)
					}
					f.git(source, "add", "feature.txt")
					f.git(source, "commit", "-m", "feature")
					f.git(source, "push", f.bare, f.Branch)
					f.head = f.git(source, "rev-parse", "HEAD")
					f.pull = true
					progress := &requestProgress{Request: monitorRequest{ID: "987-aabbccdd", Kind: "merge_manual", Repo: "owner/project", Payload: encode(map[string]any{"number": 1, "head": f.head})}, Stage: "claimed"}
					result, err := f.Executor.mergeRequestedPull(context.Background(), progress)
					if err != nil {
						t.Fatal(err)
					}
					if (result["status"] == "merged") != wantMerge {
						t.Fatalf("result: %v", result)
					}
				}
				if f.merged != wantMerge {
					t.Fatalf("merged=%v", f.merged)
				}
				if outcome == "unset" && created != 0 {
					t.Fatal("unset repo submitted native test")
				}
				if outcome != "unset" && created != 1 {
					t.Fatalf("submissions=%d", created)
				}
			})
		}
	}
}

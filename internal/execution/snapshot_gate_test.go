package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/providers"
)

type snapshotGateTransport struct {
	next       *intake.Client
	afterState func()
}

func (tr *snapshotGateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
	}
	out, err := tr.next.Explicit(req.Context(), req.Method, req.URL.RequestURI(), body)
	if err != nil {
		return nil, err
	}
	if out.Status >= 200 && out.Status < 300 && strings.HasSuffix(req.URL.Path, "/state") {
		tr.afterState()
	}
	return &http.Response{StatusCode: out.Status, Body: io.NopCloser(strings.NewReader(string(out.Body))), Request: req}, nil
}

func snapshotGateBase(t *testing.T, f *pipelineFixture, name, content string, orphan bool) {
	t.Helper()
	source := filepath.Join(f.root, "source")
	if orphan {
		f.git(source, "checkout", "--orphan", "unrelated")
		f.git(source, "rm", "-rf", ".")
	}
	if err := os.WriteFile(filepath.Join(source, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", ".")
	f.git(source, "commit", "-m", "base moves")
	f.git(source, "push", "--force", f.bare, "HEAD:refs/heads/main")
}

func TestSnapshotGate(t *testing.T) {
	for _, name := range []string{"base_only_permission", "head_permission", "base_only_sql", "head_sql", "base_only_scope", "head_scope", "no_merge_base_implement", "no_merge_base_publish", "no_merge_base_merge", "no_merge_base_resume_review", "no_merge_base_resume_publish", "no_merge_base_resume_merge", "lease_lost", "canceled", "continuation_missing"} {
		t.Run(name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fence := func(context.Context) error { return nil }
			noBase := strings.HasPrefix(name, "no_merge_base_")
			resume := strings.HasPrefix(name, "no_merge_base_resume_")
			var adds []string
			f.ScopeAdd = func(w http.ResponseWriter, paths []string) {
				adds = append(adds, paths...)
				json.NewEncoder(w).Encode(map[string]any{"result": "granted", "job": map[string]any{"id": 1, "scope": []string{"*"}}})
			}
			path, content := "custom-permission.cfg", "protected\n"
			if strings.HasSuffix(name, "sql") {
				path, content = "danger.sql", "DROP TABLE production_data;\n"
			}
			if strings.HasSuffix(name, "scope") {
				path = "outside.txt"
				var d Descriptor
				json.Unmarshal(f.Job.Raw, &d)
				d.Scope = []string{"feature.txt"}
				f.Job.Raw = encode(d)
			}
			f.Executor.Config.ReleasePolicy = &ReleasePolicy{PermissionFiles: []string{"custom-permission.cfg", "second-permission.cfg"}}
			f.CurrentPolicy = currentPolicy(f, true)
			if name == "base_only_sql" {
				snapshotGateBase(t, f, path, content, false)
				content = "SELECT 1;\n"
			}
			original := f.Executor.Call
			changed := false
			f.Executor.Call = func(ctx context.Context, request providers.Request) (providers.Answer, error) {
				answer, err := original(ctx, request)
				if !request.Review && !changed {
					changed = true
					if strings.HasPrefix(name, "head_") {
						if err := os.WriteFile(filepath.Join(request.Policy.Tree, path), []byte(content), 0600); err != nil {
							t.Fatal(err)
						}
						if name == "head_permission" {
							if err := os.WriteFile(filepath.Join(request.Policy.Tree, "second-permission.cfg"), []byte("second\n"), 0600); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				return answer, err
			}
			patches := 0
			handler := f.server.Config.Handler
			f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method == "PATCH" && strings.HasSuffix(req.URL.Path, "/issues/7") {
					patches++
				}
				handler.ServeHTTP(w, req)
			})
			moved := false
			tr := &snapshotGateTransport{next: f.API}
			tr.afterState = func() {
				if moved {
					return
				}
				r, err := f.Executor.Store.Load(1)
				if err != nil {
					t.Fatal(err)
				}
				if r == nil {
					return
				}
				target := "publish"
				if name == "no_merge_base_merge" || strings.HasPrefix(name, "base_only_") {
					target = "merge"
				}
				if r.Stage != target {
					return
				}
				if strings.HasPrefix(name, "base_only_") || name == "no_merge_base_publish" || name == "no_merge_base_merge" || name == "lease_lost" || name == "canceled" {
					moved = true
					snapshotGateBase(t, f, path, content, noBase || name == "lease_lost" || name == "canceled")
					f.git(r.Tree, "fetch", "origin")
					if strings.HasPrefix(name, "base_only_") {
						w := &gitwork.Workspace{Config: gitwork.Config{Git: f.Executor.Config.Git}, Tree: r.Tree, Default: "main"}
						snapshot, err := w.Snapshot(ctx)
						if err != nil || !slices.Equal(snapshot.Paths, []string{"feature.txt"}) {
							t.Fatalf("divergent snapshot: %+v %v", snapshot, err)
						}
					}
					if name == "lease_lost" {
						fence = func(context.Context) error { return &intake.APIError{Status: 409, Code: "lease_lost"} }
					}
					if name == "canceled" {
						cancel()
					}
				}
			}
			api, apiErr := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), tr)
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			f.Executor.API = api
			if resume {
				prepareMergeRecovery(t, f)
				r, err := f.Executor.Store.Load(1)
				if err != nil {
					t.Fatal(err)
				}
				r.Stage = strings.TrimPrefix(name, "no_merge_base_resume_")
				r.FinalState = nil
				if err := f.Executor.Store.Save(r); err != nil {
					t.Fatal(err)
				}
				snapshotGateBase(t, f, "unrelated", "root", true)
			} else if name == "no_merge_base_implement" { // Open the job tree before moving the remote to an unrelated root.
				prepareMergeRecovery(t, f)
				r, _ := f.Executor.Store.Load(1)
				r.Stage = "implement"
				r.FinalState = nil
				r.Test = nil
				r.Review = nil
				if err := f.Executor.Store.Save(r); err != nil {
					t.Fatal(err)
				}
				snapshotGateBase(t, f, "unrelated", "root", true)
			}
			if name == "continuation_missing" {
				var d Descriptor
				json.Unmarshal(f.Job.Raw, &d)
				d.HeadSHA = strings.Repeat("a", 40)
				f.Job.Raw = encode(d)
			}
			state, err := f.Executor.Execute(ctx, f.Job, func(ctx context.Context) error { return fence(ctx) })
			if name == "lease_lost" {
				var api *intake.APIError
				if !errors.As(err, &api) || !api.LeaseLost() {
					t.Fatal(state, err)
				}
				return
			}
			if name == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(state, err)
				}
				return
			}
			if err != nil {
				t.Fatal(state, err)
			}
			var view map[string]json.RawMessage
			json.Unmarshal(state.View, &view)
			switch {
			case noBase:
				if state.Status != "held" || state.HoldReason != "review_held" || !strings.Contains(string(state.Result), "マージベースを確認できません") || f.merged || patches != 0 {
					t.Fatal(state, string(state.Result), patches)
				}
			case name == "continuation_missing":
				if len(view["continuationHead"]) == 0 || f.merged {
					t.Fatal(state)
				}
			case name == "head_permission":
				var hold struct {
					Head  string
					Files []string
				}
				json.Unmarshal(view["permissionHold"], &hold)
				slices.Sort(hold.Files)
				head := f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch)
				if state.Status != "held" || state.HoldReason != "review_held" || hold.Head != head || !slices.Equal(hold.Files, []string{"custom-permission.cfg", "second-permission.cfg"}) || f.merged {
					t.Fatal(state, hold)
				}
			case name == "head_sql":
				if state.HoldReason != "deletions" || f.merged {
					t.Fatal(state)
				}
			default:
				if state.Status != "merged" || state.HoldReason == "deletions" || len(view["permissionHold"]) != 0 {
					t.Fatal(state)
				}
				if strings.HasPrefix(name, "base_only_") && slices.Contains(adds, path) {
					t.Fatal(adds)
				}
				if name == "head_scope" && !slices.Contains(adds, path) {
					t.Fatal(adds)
				}
			}
		})
	}
}

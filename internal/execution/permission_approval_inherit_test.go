package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/providers"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

const protectedBase = "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n"

// permissionInheritFixture leaves a published PR that changes protected.json
// (already present on the default branch) and waits before the merge.
// With gateOnly the Worker's own policy protects nothing, so only the gate
// before the merge (reading the current policy and job) judges the approval.
// Otherwise the publish stage judges first.
func permissionInheritFixture(t *testing.T, gateOnly bool) (*pipelineFixture, string) {
	t.Helper()
	f := newPipeline(t, "permission")
	advanceMain(f, "protected.json", protectedBase)
	implement := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, request providers.Request) (providers.Answer, error) {
		answer, err := implement(ctx, request)
		if err == nil && !request.Review {
			err = os.WriteFile(filepath.Join(request.Policy.Tree, "protected.json"), []byte(protectedBase+"added\n"), 0600)
		}
		return answer, err
	}
	if gateOnly {
		f.Executor.Config.ReleasePolicy = &ReleasePolicy{PermissionFiles: []string{}}
		prepareMergeRecovery(t, f)
		f.CurrentPolicy = currentPolicy(f, true)
		f.CurrentPolicy.ReleasePolicy = &ReleasePolicy{PermissionFiles: []string{"protected.json"}}
	} else {
		state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
		if err != nil || state.Status != "held" || heldPermissionHead(t, state.View) == "" {
			t.Fatalf("準備結果: %s %v %s", state.Status, err, state.View)
		}
		record, err := f.Executor.Store.Load(1)
		if err != nil {
			t.Fatal(err)
		}
		record.Stage = "publish"
		record.FinalState = nil
		record.FinalReported = false
		if err := f.Executor.Store.Save(record); err != nil {
			t.Fatal(err)
		}
	}
	record, err := f.Executor.Store.Load(1)
	if err != nil || record.Head == "" {
		t.Fatalf("公開済みの先頭を読めません: %v", err)
	}
	return f, record.Head
}

func advanceMain(f *pipelineFixture, file, contents string) {
	f.t.Helper()
	source := filepath.Join(f.root, "source")
	if err := os.WriteFile(filepath.Join(source, file), []byte(contents), 0600); err != nil {
		f.t.Fatal(err)
	}
	f.git(source, "add", file)
	f.git(source, "commit", "-m", "advance "+file)
	f.git(source, "push", f.bare, "main")
}

// approvePermission delivers the approval both in the leased job and in the
// job the gate re-reads, bound to the given commit.
func approvePermission(t *testing.T, f *pipelineFixture, head string) {
	t.Helper()
	view := map[string]json.RawMessage{"permissionApproval": encode(map[string]any{"id": "a", "head": head, "pr": 1, "files": []string{"protected.json"}, "resumePublication": true, "issuePosted": true, "prPosted": true, "approvalStatement": "許可", "approvedFrom": "screen"})}
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.View = view
	f.Job.Raw = encode(d)
	f.CurrentView = view
}

func heldPermissionHead(t *testing.T, view json.RawMessage) string {
	t.Helper()
	var held struct {
		Hold struct {
			Head  string   `json:"head"`
			Files []string `json:"files"`
		} `json:"permissionHold"`
	}
	_ = json.Unmarshal(view, &held)
	if !slices.Equal(held.Hold.Files, []string{"protected.json"}) {
		return ""
	}
	return held.Hold.Head
}

func implementations(f *pipelineFixture) int {
	n := 0
	for _, provider := range f.Calls {
		if provider == "codex" {
			n++
		}
	}
	return n
}

func TestPermissionApprovalIsInheritedOnlyForUnchangedProtectedFiles(t *testing.T) {
	t.Parallel()
	paths := []struct {
		name     string
		gateOnly bool
	}{{"公開時", false}, {"マージ直前と再開時", true}}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			run := func(t *testing.T, f *pipelineFixture) (string, string, json.RawMessage) {
				t.Helper()
				before := implementations(f)
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				if err != nil {
					t.Fatal(err)
				}
				if implementations(f) != before {
					t.Fatal("承認の照合のために実装をやり直しました")
				}
				record, err := f.Executor.Store.Load(1)
				if err != nil {
					t.Fatal(err)
				}
				return state.Status, record.Head, state.View
			}
			held := func(t *testing.T, f *pipelineFixture, status, head string, view json.RawMessage) {
				t.Helper()
				if status != "held" || f.merged || f.MergePosts != 0 || heldPermissionHead(t, view) != head {
					t.Fatalf("承認待ちへ戻っていません: status=%s merged=%v head=%s view=%s", status, f.merged, head, view)
				}
			}
			t.Run("同じ先頭の承認はそのまま使う", func(t *testing.T) {
				f, approved := permissionInheritFixture(t, path.gateOnly)
				approvePermission(t, f, approved)
				status, head, _ := run(t, f)
				if status != "merged" || head != approved {
					t.Fatalf("status=%s head=%s approved=%s", status, head, approved)
				}
			})
			t.Run("権限ファイルに触れない先頭更新が続いても引き継ぐ", func(t *testing.T) {
				f, approved := permissionInheritFixture(t, path.gateOnly)
				approvePermission(t, f, approved)
				advanceMain(f, "upstream1.txt", "first\n")
				var once sync.Once
				f.PullOverride = func(p *gitea.Pull) {
					if p.Head.SHA != approved {
						once.Do(func() { advanceMain(f, "upstream2.txt", "second\n") })
					}
				}
				status, head, _ := run(t, f)
				if status != "merged" || head == approved {
					t.Fatalf("status=%s head=%s approved=%s", status, head, approved)
				}
				if follows := f.git(f.root, "--git-dir", f.bare, "rev-list", "--count", "--merges", approved+"..main"); follows != "2" {
					t.Fatalf("先頭更新が2回になっていません: %s", follows)
				}
				if f.git(f.root, "--git-dir", f.bare, "show", "main:protected.json") != f.git(f.root, "--git-dir", f.bare, "show", approved+":protected.json") {
					t.Fatal("承認した内容と違う権限ファイルをマージしました")
				}
			})
			t.Run("既定ブランチの変更で権限ファイルの内容が変われば承認待ちへ戻る", func(t *testing.T) {
				f, approved := permissionInheritFixture(t, path.gateOnly)
				approvePermission(t, f, approved)
				advanceMain(f, "protected.json", "ONE\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n")
				status, head, view := run(t, f)
				if head == approved {
					t.Fatal("既定ブランチへ追従していません")
				}
				held(t, f, status, head, view)
			})
			t.Run("承認した先頭が祖先でなければ承認待ちへ戻る", func(t *testing.T) {
				f, approved := permissionInheritFixture(t, path.gateOnly)
				record, _ := f.Executor.Store.Load(1)
				sibling := f.git(record.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit-tree", approved+"^{tree}", "-p", approved+"^", "-m", "same content")
				approvePermission(t, f, sibling)
				status, head, view := run(t, f)
				held(t, f, status, head, view)
			})
			t.Run("承認した先頭に権限ファイルが無ければ承認待ちへ戻る", func(t *testing.T) {
				f, _ := permissionInheritFixture(t, path.gateOnly)
				initial := f.git(filepath.Join(f.root, "source"), "rev-list", "--max-parents=0", "HEAD")
				approvePermission(t, f, initial)
				status, head, view := run(t, f)
				held(t, f, status, head, view)
			})
		})
	}
}

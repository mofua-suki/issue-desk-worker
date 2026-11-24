package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/jobspec"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"issue-desk-worker/internal/journal"
	"testing"

	"issue-desk-worker/internal/gitea"
)

func TestFinalApprovalPublicationInputSelection(t *testing.T) {
	t.Parallel()
	head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	base := map[string]any{"generation": 2, "branch": "codex/example", "pr": 3, "headSha": head, "verified": true}
	cases := []struct {
		name, key string
		value     any
		remove    bool
		want      bool
	}{
		{name: "valid", want: true}, {name: "generation mismatch", key: "generation", value: 3}, {name: "generation missing", key: "generation", remove: true}, {name: "generation type", key: "generation", value: "2"},
		{name: "branch mismatch", key: "branch", value: "other"}, {name: "branch missing", key: "branch", remove: true}, {name: "branch type", key: "branch", value: 2},
		{name: "pr mismatch", key: "pr", value: 4}, {name: "pr missing", key: "pr", remove: true}, {name: "pr type", key: "pr", value: "3"},
		{name: "head mismatch", key: "headSha", value: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}, {name: "head missing", key: "headSha", remove: true}, {name: "head type", key: "headSha", value: 1}, {name: "invalid head", key: "headSha", value: "bad"},
		{name: "false", key: "verified", value: false}, {name: "verified missing", key: "verified", remove: true}, {name: "verified type", key: "verified", value: "true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := map[string]any{}
			for k, v := range base {
				p[k] = v
			}
			if tc.key != "" {
				if tc.remove {
					delete(p, tc.key)
				} else {
					p[tc.key] = tc.value
				}
			}
			d := Descriptor{Descriptor: jobspec.Descriptor{Generation: 2, Branch: "codex/example", PR: 3, HeadSHA: head, View: map[string]json.RawMessage{"continuation": encode(map[string]any{"publication": p})}}}
			_, got := selectFinalApprovalPublication(d)
			if got != tc.want {
				t.Fatalf("selected=%v want %v", got, tc.want)
			}
		})
	}
	for _, key := range []string{"retryFromBase", "reimplementExpected"} {
		t.Run(key, func(t *testing.T) {
			d := Descriptor{Descriptor: jobspec.Descriptor{Generation: 2, Branch: "codex/example", PR: 3, HeadSHA: head, View: map[string]json.RawMessage{"continuation": encode(map[string]any{"publication": base}), key: encode(map[string]int{"generation": 2})}}}
			if _, ok := selectFinalApprovalPublication(d); ok {
				t.Fatal("explicit retry overridden")
			}
		})
	}
	d := Descriptor{}
	if _, ok := selectFinalApprovalPublication(d); ok {
		t.Fatal("missing publication selected")
	}
	// Matching descriptor values are not sufficient when the values themselves
	// cannot identify a published branch, PR, or commit.
	for _, tc := range []struct {
		name, branch, head string
		pr                 int64
	}{
		{"invalid SHA", "codex/example", "bad", 3},
		{"invalid branch", "bad..branch", head, 3},
		{"zero PR", "codex/example", head, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Descriptor{Descriptor: jobspec.Descriptor{Generation: 2, Branch: tc.branch, PR: tc.pr, HeadSHA: tc.head, View: map[string]json.RawMessage{
				"continuation": encode(map[string]any{"publication": map[string]any{"generation": 2, "branch": tc.branch, "pr": tc.pr, "headSha": tc.head, "verified": true}}),
			}}}
			if _, ok := selectFinalApprovalPublication(d); ok {
				t.Fatal("invalid matching publication selected")
			}
		})
	}
}

func TestFinalApprovalPublicationArtifactCheck(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "approve")
	head := publishContinuationFixture(t, f)
	f.pull = true
	w := continuationWorkspace(t, f)
	d := continuationDescriptor(t, f, head)
	d.PR = 1
	d.View = map[string]json.RawMessage{"continuation": encode(map[string]any{"publication": map[string]any{"generation": d.Generation, "branch": d.Branch, "pr": d.PR, "headSha": head, "verified": true}})}
	p, ok := selectFinalApprovalPublication(d)
	if !ok {
		t.Fatal("candidate rejected")
	}
	check := func(t *testing.T, want string) {
		t.Helper()
		before := f.git(w.Tree, "rev-parse", "HEAD")
		got := f.Executor.checkFinalApprovalPublication(context.Background(), d, p, w)
		after := f.git(w.Tree, "rev-parse", "HEAD")
		if got != want {
			t.Fatalf("cause=%q want %q", got, want)
		}
		if after != before {
			t.Fatalf("check changed HEAD: before=%s after=%s", before, after)
		}
	}
	cases := []struct {
		name, cause string
		mutate      func(*gitea.Pull)
	}{
		{name: "match"},
		{name: "number", cause: "publication_pr_number_mismatch", mutate: func(p *gitea.Pull) { p.Number++ }},
		{name: "head repository", cause: "publication_head_repo_mismatch", mutate: func(p *gitea.Pull) { p.Head.Repo.FullName = "other/repo" }},
		{name: "base repository", cause: "publication_base_repo_mismatch", mutate: func(p *gitea.Pull) { p.Base.Repo.FullName = "other/repo" }},
		{name: "destination", cause: "publication_base_mismatch", mutate: func(p *gitea.Pull) { p.Base.Ref = "other" }},
		{name: "branch", cause: "publication_branch_mismatch", mutate: func(p *gitea.Pull) { p.Head.Ref = "other" }},
		{name: "pr head", cause: "publication_pr_head_mismatch", mutate: func(p *gitea.Pull) { p.Head.SHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f.PullOverride = tc.mutate
			check(t, tc.cause)
		})
	}
	f.PullOverride = nil
	// Move the fetched remote-tracking ref to a descendant: inclusion is insufficient.
	next := continuationCommit(t, f, w.Tree, "next.txt", "next\n")
	f.git(w.Tree, "update-ref", "refs/remotes/origin/"+d.Branch, next)
	check(t, "publication_branch_head_mismatch")
	f.git(w.Tree, "update-ref", "-d", "refs/remotes/origin/"+d.Branch)
	check(t, "publication_branch_unavailable")
	if len(f.Calls) != 0 || f.MergePosts != 0 || f.CreatePosts != 0 {
		t.Fatal("check changed execution or publication")
	}
	f.server.Close()
	check(t, "publication_pr_unavailable")
}

func finalPublicationPipeline(t *testing.T, mode string) (*pipelineFixture, string) {
	t.Helper()
	f := newPipeline(t, mode)
	w := continuationWorkspace(t, f)
	if mode == "permission" {
		continuationCommit(t, f, w.Tree, "protected.json", "{}\n")
	}
	if mode == "sql" {
		continuationCommit(t, f, w.Tree, "schema.sql", "DROP TABLE production_data;\n")
	}
	head := continuationCommit(t, f, w.Tree, "feature.txt", "accepted feature\n")
	if err := w.Push(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.pull, f.head = true, head
	runtime := t.TempDir()
	store, err := journal.Open(runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.Executor.Store, f.Executor.Config.RuntimeDir = store, runtime
	d := continuationDescriptor(t, f, head)
	d.PR = 1
	d.View["continuation"] = encode(map[string]any{"publication": map[string]any{"generation": d.Generation, "branch": d.Branch, "pr": d.PR, "headSha": head, "verified": true}})
	d.Result = encode(map[string]any{"implementationContent": head, "implementationResult": implementationFixture()})
	// Decode the distributed job itself, including its view and result.
	var distributed map[string]any
	if err := json.Unmarshal(encode(d), &distributed); err != nil {
		t.Fatal(err)
	}
	distributed["leaseEpoch"] = 3
	if err := json.Unmarshal(encode(distributed), &f.Job); err != nil {
		t.Fatal(err)
	}
	f.CurrentView = d.View
	return f, head
}

func TestFinalApprovalPublicationResumeAcrossHosts(t *testing.T) {
	t.Parallel()
	f, head := finalPublicationPipeline(t, "approve")
	state, err := execute(f)
	if err != nil || state.Status != "merged" || f.MergePosts != 1 || f.CreatePosts != 0 || strings.Join(f.Calls, ",") != "claude" || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
		t.Fatalf("state=%+v err=%v calls=%v merges=%d creates=%d", state, err, f.Calls, f.MergePosts, f.CreatePosts)
	}
	for _, comment := range f.Comments {
		if strings.Contains(fmt.Sprint(comment), "作業を開始しました") {
			t.Fatal("unexpected implementation start")
		}
	}
}

func TestFinalApprovalPublicationArtifactMismatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, cause string
		change      func(*gitea.Pull)
	}{
		{"number", "publication_pr_number_mismatch", func(p *gitea.Pull) { p.Number++ }},
		{"head repo", "publication_head_repo_mismatch", func(p *gitea.Pull) { p.Head.Repo.FullName = "other/repo" }},
		{"base repo", "publication_base_repo_mismatch", func(p *gitea.Pull) { p.Base.Repo.FullName = "other/repo" }},
		{"base", "publication_base_mismatch", func(p *gitea.Pull) { p.Base.Ref = "other" }},
		{"branch", "publication_branch_mismatch", func(p *gitea.Pull) { p.Head.Ref = "codex/other" }},
		{"head", "publication_pr_head_mismatch", func(p *gitea.Pull) { p.Head.SHA = strings.Repeat("b", 40) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, head := finalPublicationPipeline(t, "approve")
			f.PullOverride = tc.change
			state, err := execute(f)
			if err != nil || state.Status != "held" || state.HoldReason != "review_held" || !strings.Contains(string(state.Result), tc.cause) || len(f.Calls) != 0 || f.MergePosts != 0 || f.CreatePosts != 0 || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
				t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
			}
		})
	}
}

func TestFinalApprovalPublicationFailureHolds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, mode    string
		command       []string
		missingReport bool
	}{
		{name: "fixed test fails", mode: "approve", command: []string{"/bin/false"}},
		{name: "review returns changes", mode: "review-hold"},
		{name: "unbound report", mode: "approve", missingReport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, head := finalPublicationPipeline(t, tc.mode)
			if tc.command != nil {
				f.Executor.Config.Repositories[0].TestCommand = tc.command
			}
			if tc.missingReport {
				var d Descriptor
				_ = json.Unmarshal(f.Job.Raw, &d)
				d.Result = encode(map[string]any{"implementationContent": strings.Repeat("a", 40), "implementationResult": implementationFixture()})
				f.Job.Raw = encode(d)
			}
			state, err := execute(f)
			if err != nil || state.Status != "held" || state.HoldReason != "review_held" || !strings.Contains(string(state.Result), finalApprovalWarning) || f.MergePosts != 0 || f.CreatePosts != 0 || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
				t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
			}
			for _, call := range f.Calls {
				if call == "codex" {
					t.Fatalf("implementation called: %v", f.Calls)
				}
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil || saved == nil || saved.Stage != "held" {
				t.Fatalf("saved=%+v err=%v", saved, err)
			}
			saved.FinalReported = true
			if err := f.Executor.Store.Save(saved); err != nil {
				t.Fatal(err)
			}
			resumed, err := execute(f)
			if err != nil || resumed.Status != "held" || !strings.Contains(string(resumed.Result), finalApprovalWarning) {
				t.Fatalf("resumed=%+v err=%v", resumed, err)
			}
			for _, call := range f.Calls {
				if call == "codex" {
					t.Fatalf("implementation called on resume: %v", f.Calls)
				}
			}
		})
	}
}

func TestFinalApprovalPublicationBaseAdvanceHolds(t *testing.T) {
	t.Parallel()
	f, head := finalPublicationPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	if err := os.WriteFile(filepath.Join(source, "new.txt"), []byte("base changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "new.txt")
	f.git(source, "commit", "-m", "advance base")
	f.git(source, "push", f.bare, "main")
	state, err := execute(f)
	if err != nil || state.Status != "held" || !strings.Contains(string(state.Result), finalApprovalWarning) || len(f.Calls) != 0 || f.MergePosts != 0 || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
		t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
	}
}

func TestFinalApprovalPublicationReleaseGuards(t *testing.T) {
	t.Parallel()
	t.Run("ownership", finalApprovalPublicationOwnershipGuards)
	for _, tc := range []struct{ name, mode string }{{"missing approval", "approve"}, {"stale approval", "approve"}, {"changed branch approval", "approve"}, {"permission", "permission"}, {"deletion", "sql"}} {
		t.Run(tc.name, func(t *testing.T) {
			f, head := finalPublicationPipeline(t, tc.mode)
			switch tc.name {
			case "missing approval":
				f.CurrentView = map[string]json.RawMessage{}
			case "stale approval":
				f.CurrentView = map[string]json.RawMessage{"continuation": encode(map[string]any{"publication": map[string]any{"generation": 0, "branch": f.Branch, "pr": 1, "headSha": strings.Repeat("b", 40), "verified": true}})}
			case "changed branch approval":
				f.CurrentBranch = "codex/other"
				f.CurrentView = map[string]json.RawMessage{"continuation": encode(map[string]any{"publication": map[string]any{"generation": 0, "branch": f.CurrentBranch, "pr": 1, "headSha": head, "verified": true}})}
			}
			state, err := execute(f)
			if err != nil || state.Status != "held" || f.MergePosts != 0 || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
				t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
			}
			if tc.name == "deletion" && state.HoldReason != "deletions" {
				t.Fatalf("deletion hold=%+v", state)
			}
			for _, call := range f.Calls {
				if call == "codex" {
					t.Fatalf("implementation called: %v", f.Calls)
				}
			}
		})
	}
}

func finalApprovalPublicationOwnershipGuards(t *testing.T) {
	for _, kind := range []string{"stop", "lease", "epoch", "turn"} {
		t.Run(kind, func(t *testing.T) {
			f, _ := finalPublicationPipeline(t, "approve")
			waiting, applied := false, false
			f.PullOverride = func(p *gitea.Pull) {
				if len(f.TurnCalls) > 0 && !waiting {
					p.Mergeable = false
					waiting = true
				}
			}
			fence := func(context.Context) error {
				if !waiting || applied {
					return nil
				}
				applied = true
				switch kind {
				case "stop":
					f.Commands = []map[string]any{{"id": "stop-wait", "kind": "stop", "payload": map[string]any{"jobId": 1, "epoch": 3}}}
				case "lease":
					return errors.New("lease lost")
				case "epoch":
					f.CurrentLeaseEpoch = 4
				case "turn":
					f.TurnRenewStatus = http.StatusConflict
				}
				return nil
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, fence)
			if !applied || f.MergePosts != 0 {
				t.Fatalf("guard not exercised: state=%+v err=%v merges=%d", state, err, f.MergePosts)
			}
			for _, call := range f.Calls {
				if call == "codex" {
					t.Fatalf("implementation called: %v", f.Calls)
				}
			}
			if kind == "stop" {
				if err != nil || state.Status != "stopped" {
					t.Fatalf("state=%+v err=%v", state, err)
				}
			} else if err == nil {
				t.Fatalf("ownership loss returned success: state=%+v", state)
			}
		})
	}
}

func TestFinalApprovalPublicationRetestKeepsHead(t *testing.T) {
	t.Parallel()
	f, head := finalPublicationPipeline(t, "approve")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "echo changed >> feature.txt")
	state, err := execute(f)
	if err != nil || state.Status != "held" || f.MergePosts != 0 || len(f.Calls) != 0 || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
		t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
	}
	saved, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	var result TestResult
	_ = json.Unmarshal(saved.Test, &result)
	if result.Passed {
		t.Fatal("changed tree retained a passing result")
	}
}

func TestFinalApprovalPublicationEvidenceSelection(t *testing.T) {
	t.Parallel()
	f, head := finalPublicationPipeline(t, "approve")
	marker := filepath.Join(f.root, "test-count")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "echo x >> '"+marker+"'")
	f.CurrentView = map[string]json.RawMessage{}
	first, err := execute(f)
	if err != nil || first.Status != "held" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	d := continuationDescriptor(t, f, head)
	d.PR = 1
	var original Descriptor
	_ = json.Unmarshal(f.Job.Raw, &original)
	d.View, d.Result = original.View, first.Result
	f.Job.Raw, f.CurrentView = encode(d), d.View
	runtime := t.TempDir()
	store, err := journal.Open(runtime)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	stale := &journal.Record{ID: d.ID, Repo: d.Repo, Issue: d.Issue, Generation: d.Generation, Branch: d.Branch, PR: d.PR, Head: head, Stage: "held", Provider: d.Provider, FinalReported: true, Result: implementationFixture(), Test: encode(TestResult{Passed: true})}
	if err := store.Save(stale); err != nil {
		t.Fatal(err)
	}
	f.Executor.Store, f.Executor.Config.RuntimeDir = store, runtime
	f.Calls = nil
	second, err := execute(f)
	data, readErr := os.ReadFile(marker)
	if err != nil || readErr != nil || second.Status != "merged" || string(data) != "x\n" || strings.Join(f.Calls, ",") != "claude" || f.MergePosts != 1 {
		t.Fatalf("second=%+v err=%v marker=%q read=%v calls=%v", second, err, data, readErr, f.Calls)
	}
}

func TestFinalApprovalPublicationValidation(t *testing.T) {
	t.Parallel()
	for _, variant := range []string{"false", "absent", "generation", "branch", "pr", "headSha", "verified type", "invalid SHA"} {
		t.Run(variant, func(t *testing.T) {
			f, _ := finalPublicationPipeline(t, "approve")
			var d Descriptor
			if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
				t.Fatal(err)
			}
			if variant == "absent" {
				delete(d.View, "continuation")
			} else {
				publication := map[string]any{"generation": d.Generation, "branch": d.Branch, "pr": d.PR, "headSha": d.HeadSHA, "verified": true}
				switch variant {
				case "false":
					publication["verified"] = false
				case "generation":
					publication["generation"] = d.Generation + 1
				case "branch":
					publication["branch"] = "codex/other"
				case "pr":
					publication["pr"] = d.PR + 1
				case "headSha":
					publication["headSha"] = strings.Repeat("b", 40)
				case "verified type":
					publication["verified"] = "true"
				case "invalid SHA":
					publication["headSha"] = "invalid"
				}
				d.View["continuation"] = encode(map[string]any{"publication": publication})
			}
			f.Job.Raw = encode(d)
			calls := 0
			askingCall(f, &calls)
			state, err := execute(f)
			if err != nil || calls != 1 || state.HoldReason != "question" || f.MergePosts != 0 {
				t.Fatalf("state=%+v err=%v calls=%d", state, err, calls)
			}
		})
	}
}

func TestFinalApprovalPublicationBaseAdvanceDuringMerge(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"prepare", "refusal"} {
		t.Run(phase, func(t *testing.T) {
			f, head := finalPublicationPipeline(t, "approve")
			source := filepath.Join(f.root, "source")
			if err := os.WriteFile(filepath.Join(source, "advance.txt"), []byte("new base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(source, "add", "advance.txt")
			f.git(source, "commit", "-m", "advance")
			newBase := f.git(source, "rev-parse", "HEAD")
			if phase == "refusal" {
				f.MergeFailStatus, f.MergeFailBody = 409, `{"message":"conflict"}`
			}
			advanced := false
			f.PullOverride = func(p *gitea.Pull) {
				if !advanced && ((phase == "prepare" && f.PullReads >= 4) || (phase == "refusal" && f.MergePosts > 0)) {
					f.git(source, "push", f.bare, "main")
					p.Base.SHA = newBase
					advanced = true
				}
			}
			state, err := execute(f)
			if err != nil || !advanced || state.Status != "held" || !strings.Contains(string(state.Result), finalApprovalWarning) || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head || f.CreatePosts != 0 {
				t.Fatalf("phase=%s advanced=%v state=%+v err=%v calls=%v merges=%d", phase, advanced, state, err, f.Calls, f.MergePosts)
			}
			if phase == "prepare" && f.MergePosts != 0 {
				t.Fatal("merged after base advanced")
			}
			for _, call := range f.Calls {
				if call == "codex" {
					t.Fatalf("implementation called: %v", f.Calls)
				}
			}
		})
	}
}

func TestFinalApprovalPublicationPreservesContinuationSources(t *testing.T) {
	t.Parallel()
	f, head := finalPublicationPipeline(t, "approve")
	source := filepath.Join(f.root, "source")
	f.git(source, "checkout", "-b", "codex/other")
	if err := os.WriteFile(filepath.Join(source, "prior.txt"), []byte("unmerged work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.git(source, "add", "prior.txt")
	f.git(source, "commit", "-m", "other work")
	f.git(source, "push", f.bare, "codex/other")
	other := f.git(source, "rev-parse", "HEAD")
	if err := f.Executor.Store.Save(&journal.Record{ID: 2, Repo: "owner/project", Issue: 7, Branch: "codex/other", Head: other, Stage: "held"}); err != nil {
		t.Fatal(err)
	}
	state, err := execute(f)
	if err != nil || state.Status != "held" || len(f.Calls) != 0 || f.MergePosts != 0 || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
		t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
	}
}

// PC-A retains its journal and tree while PC-B merges the advanced base and
// publishes the exact head intake now approves. No local reset simulates resume.
func staleFinalPublicationPipeline(t *testing.T, mode string) (*pipelineFixture, *journal.Record, string) {
	t.Helper()
	f, old := finalPublicationPipeline(t, mode)
	w := continuationWorkspace(t, f)
	snapshot, err := w.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Branch: f.Branch, PR: 1, Head: old, Base: snapshot.Base, Stage: "publish", Provider: "codex", Result: reportWith("summary", "old head report")}
	fixtureRecordEvidence(t, f, r, w.Tree)
	review, raw := normalizeReview(encode(map[string]any{"decision": "approve"}))
	if err := os.MkdirAll(filepath.Dir(reviewBaselinePath(f.Executor.Config.RuntimeDir, 1)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.saveReviewBaseline(r, snapshot, review, raw, "full", reviewRequestHash(Descriptor{}, "Implement feature", "受入条件: feature.txtを生成する")); err != nil {
		t.Fatal(err)
	}
	r.Review = raw
	if err := f.Executor.Store.Save(r); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(f.root, "source")
	continuationCommit(t, f, source, "upstream.txt", "new base\n")
	f.git(source, "push", f.bare, "main")
	other := filepath.Join(f.root, "pc-b")
	f.git(f.root, "clone", f.bare, other)
	f.git(other, "checkout", f.Branch)
	f.git(other, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "merge", "--no-edit", "origin/main")
	f.git(other, "push", "origin", f.Branch)
	head := f.git(other, "rev-parse", "HEAD")
	f.head = head
	d := continuationDescriptor(t, f, head)
	d.View["continuation"] = encode(map[string]any{"publication": map[string]any{"generation": d.Generation, "branch": d.Branch, "pr": d.PR, "headSha": head, "verified": true}})
	d.Result = encode(map[string]any{"implementationContent": head, "implementationResult": reportWith("summary", "approved head report")})
	f.Job.Raw, f.CurrentView = encode(d), d.View
	if head == old || f.git(r.Tree, "rev-parse", "HEAD") != old {
		t.Fatal("fixture did not preserve PC-A's stale tree")
	}
	return f, r, head
}

func TestFinalApprovalPublicationStaleJournalResume(t *testing.T) {
	t.Parallel()
	f, before, head := staleFinalPublicationPipeline(t, "approve")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "test -f feature.txt && test \"$(cat upstream.txt)\" = 'new base'")
	state, err := execute(f)
	if err != nil || state.Status != "merged" || strings.Join(f.Calls, ",") != "claude" || f.CreatePosts != 0 || f.MergePosts != 1 || len(f.MergeHeads) != 1 || f.MergeHeads[0] != head || f.git(before.Tree, "rev-parse", "HEAD") != head || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
		t.Fatalf("state=%+v err=%v calls=%v creates=%d merges=%v", state, err, f.Calls, f.CreatePosts, f.MergeHeads)
	}
	r, err := f.Executor.Store.Load(1)
	if err != nil || r.Head != head || !strings.Contains(string(r.Result), "approved head report") {
		t.Fatalf("report target lost: record=%+v err=%v", r, err)
	}
}

func TestFinalApprovalPublicationStaleJournalMismatch(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"non ancestor", "branch", "PR", "missing saved commit", "missing approved commit", "invalid SHA", "not commit", "ancestry failure"} {
		t.Run(kind, func(t *testing.T) {
			f, r, head := staleFinalPublicationPipeline(t, "approve")
			switch kind {
			case "non ancestor":
				source := filepath.Join(f.root, "source")
				f.git(source, "checkout", "-b", "codex/unrelated")
				r.Head = continuationCommit(t, f, source, "unrelated.txt", "other work\n")
				f.git(source, "push", f.bare, "codex/unrelated")
			case "branch":
				r.Branch = "codex/other"
			case "PR":
				r.PR = 0
			case "missing saved commit":
				r.Head = strings.Repeat("f", 40)
			case "missing approved commit":
				var d Descriptor
				_ = json.Unmarshal(f.Job.Raw, &d)
				d.HeadSHA = strings.Repeat("f", 40)
				d.View["continuation"] = encode(map[string]any{"publication": map[string]any{"generation": d.Generation, "branch": d.Branch, "pr": d.PR, "headSha": d.HeadSHA, "verified": true}})
				f.Job.Raw = encode(d)
			case "invalid SHA":
				r.Head = "HEAD~1"
			case "not commit":
				r.Head = f.git(r.Tree, "rev-parse", "HEAD^{tree}")
			case "ancestry failure":
				continuationGitFailure(t, f, "merge-base --is-ancestor "+r.Head+" "+head)
			}
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			local := f.git(r.Tree, "rev-parse", "HEAD")
			state, err := execute(f)
			if err != nil || state.Status != "held" || state.HoldReason != "review_held" || !strings.Contains(string(state.Result), "保存された公開対象が配布対象と一致しません") || len(f.Calls) != 0 || f.CreatePosts != 0 || f.MergePosts != 0 || f.git(r.Tree, "rev-parse", "HEAD") != local || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
				t.Fatalf("state=%+v err=%v calls=%v creates=%d merges=%d", state, err, f.Calls, f.CreatePosts, f.MergePosts)
			}
		})
	}
}

func TestFinalApprovalPublicationStaleJournalWorkspaceGuards(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"dirty tracked", "dirty untracked", "diverged", "follow failure", "status failure", "dirty during fence", "stop", "lease", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f, r, head := staleFinalPublicationPipeline(t, "approve")
			file := ""
			switch kind {
			case "dirty tracked", "dirty during fence":
				file = "feature.txt"
			case "dirty untracked":
				file = "pending.txt"
			case "diverged":
				continuationCommit(t, f, r.Tree, "local.txt", "unpublished work\n")
			case "follow failure":
				continuationGitFailure(t, f, "merge --ff-only")
			case "status failure":
				continuationGitFailure(t, f, "status --porcelain")
			}
			if file != "" && kind != "dirty during fence" {
				if err := os.WriteFile(filepath.Join(r.Tree, file), []byte("pending work\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			local := f.git(r.Tree, "rev-parse", "HEAD")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fences := 0
			lost := errors.New("fixture ownership lost: " + kind)
			state, err := f.Executor.Execute(ctx, f.Job, func(ctx context.Context) error {
				fences++
				switch kind {
				case "dirty during fence":
					return os.WriteFile(filepath.Join(r.Tree, file), []byte("pending work\n"), 0600)
				case "stop":
					f.Commands = []map[string]any{{"id": "stop-follow", "kind": "stop", "payload": map[string]any{"jobId": 1, "epoch": 3}}}
					return f.Executor.pollCommands(ctx)
				case "lease":
					return lost
				case "cancel":
					cancel()
				}
				return nil
			})
			switch kind {
			case "stop":
				if fences != 1 || err != nil || state.Status != "stopped" {
					t.Fatalf("stop bypassed: state=%+v err=%v fences=%d", state, err, fences)
				}
			case "lease":
				if fences != 1 || !errors.Is(err, lost) {
					t.Fatalf("ownership bypassed: %v fences=%d", err, fences)
				}
			case "cancel":
				if fences != 1 || !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel bypassed: %v fences=%d", err, fences)
				}
			default:
				if err != nil || state.Status != "held" || state.HoldReason != "review_held" {
					t.Fatalf("state=%+v err=%v", state, err)
				}
			}
			if len(f.Calls) != 0 || f.CreatePosts != 0 || f.MergePosts != 0 || f.git(r.Tree, "rev-parse", "HEAD") != local || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
				t.Fatalf("work or publication changed: calls=%v creates=%d merges=%d", f.Calls, f.CreatePosts, f.MergePosts)
			}
			if file != "" {
				data, err := os.ReadFile(filepath.Join(r.Tree, file))
				if err != nil || string(data) != "pending work\n" {
					t.Fatalf("pending work lost: %q %v", data, err)
				}
			}
		})
	}
}

func TestFinalApprovalPublicationStaleJournalEvidence(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"missing report", "old delivered report", "invalid delivered report", "old test", "old review"} {
		t.Run(kind, func(t *testing.T) {
			mode := "approve"
			if kind == "old review" {
				mode = "review-hold"
			}
			f, r, head := staleFinalPublicationPipeline(t, mode)
			var d Descriptor
			_ = json.Unmarshal(f.Job.Raw, &d)
			switch kind {
			case "missing report":
				d.Result = nil
			case "old delivered report":
				d.Result = encode(map[string]any{"implementationContent": r.Head, "implementationResult": r.Result})
			case "invalid delivered report":
				d.Result = encode(map[string]any{"implementationContent": head, "implementationResult": reportWith("status", "question")})
			case "old test":
				// Both saved and transported old passes must be rejected.
				d.Result = encode(map[string]any{"implementationContent": head, "implementationResult": implementationFixture(), "fixedTest": r.Test})
				f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "exit 1")
			case "old review":
				// Even a valid transported test for the new head cannot make the
				// saved old-head approval valid for it.
				current := &journal.Record{ID: 1, Repo: r.Repo, Tree: filepath.Join(f.root, "pc-b"), Head: head, Base: f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/main"), Result: implementationFixture()}
				fixtureRecordEvidence(t, f, current, current.Tree)
				d.Result = encode(map[string]any{"implementationContent": head, "implementationResult": implementationFixture(), "fixedTest": current.Test})
			}
			f.Job.Raw = encode(d)
			state, err := execute(f)
			if err != nil || state.Status != "held" || state.HoldReason != "review_held" || f.MergePosts != 0 || f.CreatePosts != 0 || f.git(r.Tree, "rev-parse", "HEAD") != head || f.git(f.root, "--git-dir", f.bare, "rev-parse", "refs/heads/"+f.Branch) != head {
				t.Fatalf("state=%+v err=%v calls=%v", state, err, f.Calls)
			}
			if kind == "old review" {
				if strings.Join(f.Calls, ",") != "claude" {
					t.Fatalf("new review not requested: %v", f.Calls)
				}
			} else if len(f.Calls) != 0 {
				t.Fatalf("unexpected AI: %v", f.Calls)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil || saved.Head != head {
				t.Fatalf("target=%+v err=%v", saved, err)
			}
			if strings.Contains(kind, "report") && hasJSON(saved.Result) {
				t.Fatalf("old report adopted: result=%s previous=%s", saved.Result, saved.PreviousResult)
			}
			if kind == "old test" {
				var test TestResult
				if json.Unmarshal(saved.Test, &test) != nil || test.Passed {
					t.Fatalf("old pass reused: %s", saved.Test)
				}
			}
		})
	}
}

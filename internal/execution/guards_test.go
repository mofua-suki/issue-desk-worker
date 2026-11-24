package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPermissionApprovalRequiresSameHeadAndCompleteSet(t *testing.T) {
	view := map[string]json.RawMessage{"permissionApproval": json.RawMessage(`{"id":"approval","head":"abc","pr":7,"files":["a","b"],"resumePublication":true,"issuePosted":true,"prPosted":true,"approvalStatement":"対象を承認します","approvedFrom":"screen"}`)}
	d := Descriptor{Descriptor: jobspec.Descriptor{View: view}}
	w := &gitwork.Workspace{Config: gitwork.Config{Git: "git"}, Tree: t.TempDir()}
	if !permissionApprovedContent(context.Background(), w, d, "abc", 7, []string{"b", "a"}) {
		t.Fatal("同じ対象の承認を読めません")
	}
	if permissionApprovedContent(context.Background(), w, d, "other", 7, []string{"a", "b"}) ||
		permissionApprovedContent(context.Background(), w, d, "abc", 7, []string{"a"}) ||
		permissionApprovedContent(context.Background(), w, d, "abc", 8, []string{"a", "b"}) {
		t.Fatal("確認できない先頭、対象集合またはPRが違う承認を使いました")
	}
}
func TestSQLGuardMarkdownAndExecutableFiles(t *testing.T) {
	for _, tc := range []struct {
		path string
		hold bool
	}{
		{"docs/chat-operations.md", false},
		{"AGENTS.md", true},
		{"src/Agents.md", true},
		{"CLAUDE.md", true},
		{"GEMINI.md", true},
		{".claude/commands/prune.md", true},
		{".claude/agents/prune.md", true},
		{".claude/rules/db.md", true},
		{".claude/skills/db/SKILL.md", true},
		{".github/copilot-instructions.md", true},
		{".github/instructions/db.md", true},
		{".github/prompts/prune.md", true},
		{".cursor/rules/db.md", true},
		{"test/AGENTS.md", true},
		{"test/.claude/commands/db.md", true},
		{"docs/.claude-guide.md", false},
		{"docs/.claude/commands-extra/db.md", false},
		{"db/cleanup.sql", true},
		{"internal/store.go", true},
		{"scripts/cleanup.sh", true},
		{"config/cleanup.conf", true},
		{"server/src/prune-db.mjs", true},
		{"docs/example.md.go", true},
		{"docs/example.MD", true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			s := gitwork.Snapshot{Paths: []string{tc.path}, Diff: "diff --git a/" + tc.path + " b/" + tc.path + "\n@@ -0,0 +1 @@\n+DELETE FROM requests\n"}
			if got := sqlHold(s); got != tc.hold {
				t.Fatalf("sqlHold = %v, want %v", got, tc.hold)
			}
		})
	}
}

func TestSQLGuardMarkdownDoesNotHideOtherFiles(t *testing.T) {
	s := gitwork.Snapshot{
		Paths: []string{"docs/chat-operations.md", "internal/store.go"},
		Diff: "diff --git a/docs/chat-operations.md b/docs/chat-operations.md\n@@ -0,0 +1 @@\n+DELETE FROM requests\n" +
			"diff --git a/internal/store.go b/internal/store.go\n@@ -0,0 +1 @@\n+DELETE FROM requests\n",
	}
	if !sqlHold(s) {
		t.Fatal("Markdownと同じ差分のGoファイルの削除SQLを保留しません")
	}
}

func TestPublicationRejectsCredentialsAndOutOfScope(t *testing.T) {
	if validateSnapshot(gitwork.Snapshot{Paths: []string{"private/token"}}) == nil {
		t.Fatal("秘密ファイルを公開可能としました")
	}
	if scopeContains([]string{"server/src"}, "server/srcx/key.go") {
		t.Fatal("名前の前方一致で範囲外を許しました")
	}
	if !scopeContains([]string{"server/src"}, "server/src/jobs.go") {
		t.Fatal("子の範囲を認識しません")
	}
}
func TestSQLGuardIncludesRowDeletionAndMultilineStatements(t *testing.T) {
	for _, sql := range []string{"DELETE FROM requests", "delete \n+from requests", "TRUNCATE TABLE data", "DROP TABLE data", "DROP \n+TABLE data"} {
		snapshot := gitwork.Snapshot{Paths: []string{"internal/store.go"}, Diff: "diff --git a/internal/store.go b/internal/store.go\n@@ -0,0 +1 @@\n+" + sql + "\n"}
		if !sqlHold(snapshot) {
			t.Fatalf("削除SQLを保留しません: %q", sql)
		}
		snapshot.Paths = []string{"internal/store_test.go"}
		snapshot.Diff = strings.ReplaceAll(snapshot.Diff, "internal/store.go", "internal/store_test.go")
		if sqlHold(snapshot) {
			t.Fatal("一時テストだけの変更を保留しました")
		}
	}
}

func TestPermissionPolicySelectsRepositoryAndFailsClosedForUnknown(t *testing.T) {
	e := &Executor{Config: Configuration{Fleet: Fleet{ReleasePolicy: &ReleasePolicy{PermissionFiles: []string{"fallback"}, Repositories: map[string]RepositoryReleasePolicy{
		"owner/worker": {PermissionFiles: []string{"worker-guard.go"}},
		"owner/intake": {PermissionFiles: []string{"server-guard.go"}},
	}}}}}
	snapshot := gitwork.Snapshot{Paths: []string{"worker-guard.go", "server-guard.go", "fallback"}}
	left, err := e.permissionFiles(snapshot, "owner/worker")
	if err != nil || len(left) != 1 || left[0] != "worker-guard.go" {
		t.Fatal("Worker以外の保護集合を使いました", left, err)
	}
	right, err := e.permissionFiles(snapshot, "owner/intake")
	if err != nil || len(right) != 1 || right[0] != "server-guard.go" {
		t.Fatal("Intake以外の保護集合を使いました", right, err)
	}
	if _, err := e.permissionFiles(snapshot, "owner/unknown"); err == nil {
		t.Fatal("未配信のrepoへ共通集合を代用しました")
	}
	if len(e.Config.ReleasePolicy.Repositories["owner/worker"].PermissionFiles) != 1 {
		t.Fatal("共有設定を書き換えました")
	}
}

func TestReviewClassificationAndLegacy(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		approved bool
	}{
		{"user", `{"decision":"approve","userJudgments":["選択"]}`, false},
		{"legacy", `{"decision":"approve","uncertainties":["旧判断"]}`, false},
		{"breaking", `{"decision":"approve","breaking":true}`, false},
		{"resolvable", `{"decision":"hold","resolvableUncertainties":["追加確認"]}`, true},
		{"must", `{"decision":"hold","findings":["修正"],"findingKinds":["other"],"findingSeverities":["must"],"resolvableUncertainties":["確認"]}`, false},
		{"empty", `{"decision":"approve"}`, true},
		{"minorHold", `{"decision":"hold","findings":["軽微"],"findingKinds":["other"],"findingSeverities":["minor"]}`, true},
		{"tooLarge", `{"decision":"approve","tooLarge":true}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, raw := normalizeReview(json.RawMessage(tc.raw))
			if r.Approved() != tc.approved {
				t.Fatal(r)
			}
			if tc.name == "resolvable" && !strings.Contains(string(raw), `"improvements":["追加確認"]`) {
				t.Fatal(string(raw))
			}
			if tc.name == "must" && (!strings.Contains(string(raw), "確認") || !strings.Contains(string(raw), "修正")) {
				t.Fatal(string(raw))
			}
		})
	}
}

func TestReviewHoldSummaryPlacesReasonsFirst(t *testing.T) {
	r := Review{Summary: strings.Repeat("長", 3000), UserJudgments: []string{"選択してください"}}
	summary := clip(r.holdSummary(), 2000)
	if !strings.HasPrefix(summary, "利用者の判断: 選択してください") {
		t.Fatal(summary)
	}
}

func TestReviewPromptMatchesShippedSchema(t *testing.T) {
	prompt := reviewPrompt(Descriptor{}, "", "", &journal.Record{}, gitwork.Snapshot{}, "diff")
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(providers.Schema(true), &schema); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"userJudgments", "resolvableUncertainties", "recordOnly"} {
		if !strings.Contains(prompt, name) || !slices.Contains(schema.Required, name) || schema.Properties[name] == nil {
			t.Fatalf("契約に %s がありません", name)
		}
	}
	if slices.Contains(schema.Required, "uncertainties") || schema.Properties["uncertainties"] != nil {
		t.Fatal("旧欄を新規回答に要求しました")
	}
	// 留意点の置き場は、プロンプトと回答契約の説明の両方でAIへ伝える。説明を足しても回答の検証は変わらない。
	for _, name := range []string{"postMergeChecks", "userJudgments", "resolvableUncertainties"} {
		var field struct {
			Description string `json:"description"`
		}
		if err := json.Unmarshal(schema.Properties[name], &field); err != nil || field.Description == "" || !strings.Contains(prompt, name) {
			t.Fatalf("%s の説明がありません: %v", name, err)
		}
	}
}

func TestPermissionApprovedContentKeepsHoldWithoutAValidRecord(t *testing.T) {
	d := Descriptor{Descriptor: jobspec.Descriptor{View: map[string]json.RawMessage{"permissionApproval": json.RawMessage(`{"id":"approval","head":"abc","pr":7,"files":["a"],"resumePublication":true,"issuePosted":true,"prPosted":false,"approvalStatement":"ok","approvedFrom":"token"}`)}}}
	if permissionApprovedContent(context.Background(), nil, d, "abc", 7, []string{"a"}) {
		t.Fatal("投稿が済んでいない承認を使いました")
	}
	d.View["permissionApproval"] = json.RawMessage(`{"id":"approval","head":"abc","pr":7,"files":["a"],"resumePublication":true,"issuePosted":true,"prPosted":true,"approvalStatement":"ok","approvedFrom":"token"}`)
	if !permissionApprovedContent(context.Background(), nil, d, "abc", 7, []string{"a"}) {
		t.Fatal("同じ先頭の承認を読めません")
	}
	if permissionApprovedContent(context.Background(), nil, d, "other", 7, []string{"a"}) {
		t.Fatal("内容を照合できない別の先頭へ承認を引き継ぎました")
	}
}

func TestSecretPublicationBoundaries(t *testing.T) {
	cases := []struct {
		name, value string
		reject      bool
	}{
		{"claude20", "sk-ant-" + "Abcdefghij_12345678-", true},
		{"claude_after_letter", "xsk-ant-" + strings.Repeat("a", 20), true},
		{"claude_after_digit", "9sk-ant-" + strings.Repeat("a", 20), true},
		{"claude_after_underscore", "_sk-ant-" + strings.Repeat("a", 20), true},
		{"claude19", "sk-ant-" + strings.Repeat("a", 19), false},
		{"idk", "idk_" + strings.Repeat("a", 20), true},
		{"idh", "idh_" + strings.Repeat("a", 20), true},
		{"gh", "ghp_" + strings.Repeat("a", 20), true},
		{"normal", "ordinary change", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := gitwork.Snapshot{Paths: []string{"file.txt"}, Diff: "--- a/file.txt\n+++ b/file.txt\n+" + tc.value + "\n"}
			if got := validateSnapshot(snapshot) != nil; got != tc.reject {
				t.Fatalf("added line rejected = %v", got)
			}
			snapshot.Diff = "-" + tc.value + "\n"
			if err := validateSnapshot(snapshot); err != nil {
				t.Fatalf("removed line: %v", err)
			}
			root := t.TempDir()
			if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
				t.Fatalf("git init: %s: %v", out, err)
			}
			// Place the token across the reader's 32 KiB boundary to exercise whole-file scanning.
			if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte(strings.Repeat("x", 32760)+" "+tc.value), 0600); err != nil {
				t.Fatal(err)
			}
			w := &gitwork.Workspace{Config: gitwork.Config{Git: "git"}, Tree: root}
			if got := (&Executor{}).validatePending(context.Background(), w) != nil; got != tc.reject {
				t.Fatalf("file rejected = %v", got)
			}
		})
	}
}

func TestSecretInPlusPrefixedAddedLineBlocksMerge(t *testing.T) {
	secret := "sk-ant-" + strings.Repeat("a", 20)
	snapshot := gitwork.Snapshot{Paths: []string{"file.txt"}, Diff: "diff --git a/file.txt b/file.txt\n--- a/file.txt\n+++ b/file.txt\n@@ -0,0 +1 @@\n+++" + secret + "\n"}
	if err := validateSnapshot(snapshot); err == nil {
		t.Fatal("plus-prefixed added line accepted")
	}
	result, err := (&Executor{}).checkMergeSnapshotData(snapshot, "owner/repo", &mergeGate{})
	if err != nil || result["status"] != "refused" || result["message"] != "追加行に資格情報らしい値があります" {
		t.Fatalf("merge result = %v, %v", result, err)
	}
	// A real file header can contain the same text without being an added line.
	snapshot.Diff = "diff --git a/file b/file\n--- a/file\n+++ b/" + secret + "\n@@ -0,0 +1 @@\n+ordinary\n"
	if err := validateSnapshot(snapshot); err != nil {
		t.Fatalf("file header rejected: %v", err)
	}
}

func TestChangedFilesPassOwnPublicationGuard(t *testing.T) {
	root := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	for _, name := range []string{"guards.go", "tests.go", "guards_test.go", "host_holds_test.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	w := &gitwork.Workspace{Config: gitwork.Config{Git: "git"}, Tree: root}
	if err := (&Executor{}).validatePending(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}

func TestRedactSecretForms(t *testing.T) {
	for _, value := range []string{"sk-ant-" + strings.Repeat("a", 20), "sk-ant-short", "idk_short", "idh_short"} {
		if got := redactKnown("before " + value + " after"); got != "before [REDACTED] after" {
			t.Fatalf("redact = %q", got)
		}
	}
	for _, prefix := range []string{"x", "9", "_"} {
		if got := redactKnown(prefix + "sk-ant-" + strings.Repeat("a", 20)); got != prefix+"[REDACTED]" {
			t.Fatalf("redact = %q", got)
		}
	}
	if got := redactKnown("ordinary change"); got != "ordinary change" {
		t.Fatal(got)
	}
}

func TestSQLGuardQuotedDocumentPaths(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
		return string(out)
	}
	git("init", "-q")
	paths := []string{"資料.md", "処理.go", "指示/AGENTS.md"}
	for _, name := range paths {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("before\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "--", ".")
	for _, name := range paths {
		if err := os.WriteFile(filepath.Join(root, name), []byte("before\nDELETE FROM requests\n"), 0600); err != nil {
			t.Fatal(err)
		}
		diff := git("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--", name)
		if !strings.HasPrefix(diff, "diff --git \"a/") {
			t.Fatalf("Git did not quote the header: %q", diff)
		}
		snapshot := gitwork.Snapshot{Paths: []string{name}, Diff: diff}
		if got, want := sqlHold(snapshot), name != paths[0]; got != want {
			t.Fatalf("%s: sqlHold = %v, want %v", name, got, want)
		}
		if name == paths[0] {
			header, rest, _ := strings.Cut(diff, "\n")
			for _, bad := range []string{
				strings.Replace(header, `\350`, `\q`, 1),
				strings.Replace(header, `"b/`, `"b/other`, 1),
			} {
				if bad == header {
					t.Fatalf("failed to construct invalid header: %s", header)
				}
				snapshot.Diff = bad + "\n" + rest
				if !sqlHold(snapshot) {
					t.Fatalf("unrecognized header did not hold: %s", bad)
				}
			}
		}
	}
}

package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/journal"
)

func TestPrettierFailureDetail(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0700); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(tree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "package.json"), []byte(`{"devDependencies":{"prettier":"3.6.2"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(dir, "sandbox")
	token := "idk_syntheticSecret123"
	script := "#!/bin/sh\nprintf '%s\\n' 'format output'\nprintf '%s\\n' '[error] docs/x.md: SyntaxError ... " + token + "' >&2\nexit 2\n"
	if err := os.WriteFile(sandbox, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: dir, Codex: sandbox, PNPM: "/bin/true"}}}
	result, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{"/bin/true"}, TestTimeoutSeconds: 10}, &journal.Record{Tree: tree, Result: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed || !strings.HasPrefix(result.Detail, "整形が失敗しました") || !strings.Contains(result.Detail, "終了コード 2") || !strings.Contains(result.Detail, "[error] docs/x.md: SyntaxError ...") || !strings.Contains(result.Detail, "format output") || strings.Contains(result.Detail, token) || !strings.Contains(result.Detail, "[REDACTED]") {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestGoFormatFailureDetail(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0700); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(tree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module example.test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(dir, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nprintf 'format stdout\\n'\nprintf 'broken.go: syntax error idk_secret123\\n' >&2\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: dir, Codex: sandbox}}}
	result, err := e.fixedTest(context.Background(), Repository{TestCommand: []string{"/bin/true"}, TestTimeoutSeconds: 10}, &journal.Record{Tree: tree, Result: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed || !strings.Contains(result.Detail, "終了コード 2") || !strings.Contains(result.Detail, "format stdout") || !strings.Contains(result.Detail, "broken.go: syntax error") || strings.Contains(result.Detail, "idk_secret123") || !result.evidenceReason {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestSetupCommandFailureDetail(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "workspaces"), 0700); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(tree, 0700); err != nil {
		t.Fatal(err)
	}
	sandbox := filepath.Join(dir, "sandbox")
	if err := os.WriteFile(sandbox, []byte("#!/bin/sh\nprintf 'setup stdout\\n'\nprintf 'setup error idk_secret123\\n' >&2\nexit 3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	e := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: dir, Codex: sandbox}}}
	err := e.setup(context.Background(), Repository{SetupCommand: []string{"/bin/false"}}, &journal.Record{Tree: tree})
	if err == nil || !strings.Contains(err.Error(), "終了コード 3") || !strings.Contains(err.Error(), "setup stdout") || !strings.Contains(err.Error(), "setup error") || strings.Contains(err.Error(), "idk_secret123") {
		t.Fatalf("unexpected error: %v", err)
	}
}

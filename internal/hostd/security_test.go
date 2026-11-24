package hostd

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/process"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppFilesCannotEscapeAfterCheckout(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("services: {}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	app := App{Repo: "owner/project", Path: root, ComposeFiles: []string{"compose.yaml"}, Health: json.RawMessage(`["true"]`)}
	if _, err := appConfig(app, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "compose.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := appConfig(app, true); err == nil {
		t.Fatal("checkout後の外向きsymlinkを許可しました")
	}
}
func TestLiveResultRejectsSecretsRawDataAndUnknownFields(t *testing.T) {
	for _, raw := range []string{
		`{"pass":true,"summary":"https://example.test/?secret=abc","metrics":{},"failures":[]}`,
		`{"pass":true,"summary":"ok","metrics":{},"failures":[],"image":"base64"}`,
		`{"pass":true,"summary":"ok","metrics":{},"failures":[{"target":"x","reason":"failure"}]}`,
		`{"pass":false,"summary":"ok","metrics":{},"failures":[]}`,
	} {
		if _, err := validateLive([]byte(raw)); err == nil {
			t.Fatal("危険または不整合の結果を許可しました")
		}
	}
	if _, err := validateLive([]byte(`{"pass":true,"summary":"正常","metrics":{"count":0},"failures":[]}`)); err != nil {
		t.Fatal(err)
	}
}
func TestLiveCheckNeverRunsRemoteCommandOutsideLocalAllowlist(t *testing.T) {
	home := t.TempDir()
	calls := 0
	posted := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "live-check-next") {
			json.NewEncoder(w).Encode(map[string]any{"job": map[string]any{"id": 7, "repo": "owner/project", "commit": strings.Repeat("a", 40), "host": "test", "command": []string{"unapproved"}, "timeoutSeconds": 10}})
			return
		}
		if strings.HasSuffix(req.URL.Path, "live-check-result") {
			var body livePending
			if json.NewDecoder(req.Body).Decode(&body) != nil || body.Result.Pass || len(body.Result.Failures) != 1 || body.Result.Failures[0].Reason != "not_allowed" {
				t.Error("invalid refusal")
			}
			posted = true
			w.Write([]byte("{}"))
			return
		}
		t.Error("unexpected endpoint")
		w.WriteHeader(404)
	}))
	defer server.Close()
	r := &Runtime{Config: Configuration{URL: server.URL, Host: "test", Home: home, LiveChecks: []LiveCheck{{Repo: "owner/project", Path: home, Command: []string{"approved"}, TimeoutSeconds: 10}}}, HTTP: server.Client(), token: "idh_" + strings.Repeat("a", 43), Now: time.Now, Run: func(context.Context, string, []string, process.Options) (process.Result, error) {
		calls++
		return process.Result{}, nil
	}}
	if err := r.runLiveChecks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !posted {
		t.Fatal("remote command ran or refusal not reported")
	}
}
func TestHostdCredentialRejectsSymlinkAndSharedMode(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "token")
	if err := os.WriteFile(file, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	// WriteFile の権限は umask で削られるので、共有の権限は Chmod で作る。
	if err := os.Chmod(file, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := secret(file, nil); err == nil {
		t.Fatal("shared credential accepted")
	}
	os.Chmod(file, 0600)
	if _, err := secret(file, nil); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	os.Symlink(file, link)
	if _, err := secret(link, nil); err == nil {
		t.Fatal("credential symlink accepted")
	}
}

package apple

import (
	"bytes"
	"context"
	"errors"
	"issue-desk-worker/internal/fleetconfig"
	"issue-desk-worker/internal/nativetest"
	"issue-desk-worker/internal/process"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/failurelog"
	"issue-desk-worker/internal/intake"
)

func appleRoundRunner(t *testing.T, respond func(*http.Request) *http.Response) *Runner {
	t.Helper()
	c := fleetconfig.Configuration{Host: "apple", Version: "v1", Fleet: fleetconfig.Fleet{
		Git: "/missing/git", GiteaURL: "https://git.invalid", WorkerTokenFile: "/private/git-token", HostRole: "test-only", TestRepositories: []string{"owner/project"},
		Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}},
	}}
	api, err := intake.New(config.Bootstrap{ServerURL: "https://intake.invalid", WorkerID: "apple", ServerTokenFile: "unused"}, "idk_"+strings.Repeat("a", 43),
		nativeTransport(func(r *http.Request) (*http.Response, error) { return respond(r), nil }))
	if err != nil {
		t.Fatal(err)
	}
	return &Runner{Root: t.TempDir(), API: api, Config: c}
}

// 受付の一時的な障害や1件の不正な依頼状態では、Apple runnerを終えずに次の周期へ回す。
func TestAppleRoundKeepsRunnerOnTemporaryFailures(t *testing.T) {
	heartbeatFails := true
	var nexts int
	a := appleRoundRunner(t, func(r *http.Request) *http.Response {
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/hosts/"):
			if heartbeatFails {
				return nativeResponse(503, map[string]any{})
			}
			return nativeResponse(200, map[string]any{})
		case strings.HasSuffix(r.URL.Path, "/client-config"):
			return nativeResponse(200, intake.ConfigSnapshot{Version: "v1", SettingsVersion: "s", ConfigurationVersion: "v1", Config: encode(fleetconfig.Fleet{
				Git: "/missing/git", GiteaURL: "https://git.invalid", WorkerTokenFile: "/private/git-token", HostRole: "test-only", TestRepositories: []string{"owner/project"},
				Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}},
			})})
		case strings.HasSuffix(r.URL.Path, "/next"):
			nexts++
			return nativeResponse(200, map[string]any{"request": nil})
		}
		t.Errorf("予期しない経路: %s", r.URL.Path)
		return nativeResponse(404, map[string]any{})
	})
	var log bytes.Buffer
	failures := failurelog.Log{Out: &log, Label: "Apple runnerの周期を完了できませんでした"}
	if err := a.round(context.Background(), &failures); err != nil {
		t.Fatalf("heartbeatの一時的な失敗でrunnerを終えました: %v", err)
	}
	if !strings.Contains(log.String(), "Apple runnerの周期を完了できませんでした") {
		t.Fatalf("失敗を記録していません: %q", log.String())
	}
	heartbeatFails = false
	dir := filepath.Join(a.Root, "control", "state", "requests")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.round(context.Background(), &failures); err != nil {
		t.Fatalf("不正な依頼状態でrunnerを終えました: %v", err)
	}
	if raw, err := os.ReadFile(broken + ".broken"); err != nil || string(raw) != "{broken" || nexts != 1 {
		t.Fatalf("不正な状態を退避して次の依頼へ進んでいません: %q %v next=%d", raw, err, nexts)
	}
}

// 受付が資格情報を断ったときは、再試行で直らないのでrunnerを終える。
func TestAppleRoundEndsRunnerWhenCredentialIsRefused(t *testing.T) {
	a := appleRoundRunner(t, func(r *http.Request) *http.Response { return nativeResponse(401, map[string]any{}) })
	var restart *appleRestart
	if err := a.round(context.Background(), &failurelog.Log{Out: &bytes.Buffer{}}); !errors.As(err, &restart) {
		t.Fatalf("断られた資格情報で周期を続けました: %v", err)
	}
}

// 2回目の不正な状態は、前に退避した中身を上書きしない。
func TestAppleRoundKeepsEarlierBrokenState(t *testing.T) {
	a := appleRoundRunner(t, func(r *http.Request) *http.Response { return nativeResponse(200, map[string]any{}) })
	dir := filepath.Join(a.Root, "control", "state", "requests")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.json")
	for _, body := range []string{"{first", "{second"} {
		if err := os.WriteFile(broken, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_ = a.cycle(context.Background()) // 退避の後、空の設定で失敗してよい

	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("退避した状態が2つ残っていません: %v %v", entries, err)
	}
	if raw, err := os.ReadFile(broken + ".broken"); err != nil || string(raw) != "{first" {
		t.Fatalf("前の退避を上書きしました: %q %v", raw, err)
	}
}

// 資格情報の設定が変わったときは、起動時の検査をやり直すためにrunnerを終える。
func TestAppleRoundEndsRunnerWhenCredentialsChange(t *testing.T) {
	a := appleRoundRunner(t, func(r *http.Request) *http.Response {
		if strings.HasSuffix(r.URL.Path, "/client-config") {
			return nativeResponse(200, intake.ConfigSnapshot{Version: "v1", SettingsVersion: "s", ConfigurationVersion: "v1", Config: encode(fleetconfig.Fleet{
				Git: "/missing/git", GiteaURL: "https://other.invalid", WorkerTokenFile: "/private/git-token", HostRole: "test-only", TestRepositories: []string{"owner/project"},
				Repositories: []fleetconfig.Repository{{Name: "owner/project", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}},
			})})
		}
		return nativeResponse(200, map[string]any{})
	})
	var restart *appleRestart
	if err := a.round(context.Background(), &failurelog.Log{Out: &bytes.Buffer{}}); !errors.As(err, &restart) {
		t.Fatalf("資格情報の変更でrunnerを終えません: %v", err)
	}
}

// 1つのリポジトリの依頼の取得に失敗しても、残りのリポジトリには問い合わせる。
func TestCycleAsksOtherRepositoriesAfterOneFails(t *testing.T) {
	fleet := fleetconfig.Fleet{
		Git: "/missing/git", GiteaURL: "https://git.invalid", WorkerTokenFile: "/private/git-token", HostRole: "test-only", TestRepositories: []string{"owner/first", "owner/second"},
		Repositories: []fleetconfig.Repository{{Name: "owner/first", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}, {Name: "owner/second", NativeTestHost: "apple", TestCommand: []string{"/bin/true"}}},
	}
	var asked []string
	a := appleRoundRunner(t, func(r *http.Request) *http.Response {
		switch {
		case strings.HasSuffix(r.URL.Path, "/client-config"):
			return nativeResponse(200, intake.ConfigSnapshot{Version: "v1", SettingsVersion: "s", ConfigurationVersion: "v1", Config: encode(fleet)})
		case strings.HasSuffix(r.URL.Path, "/next"):
			asked = append(asked, r.URL.Query().Get("repo"))
			if r.URL.Query().Get("repo") == "owner/first" {
				return nativeResponse(503, map[string]any{})
			}
			return nativeResponse(200, map[string]any{"request": nil})
		}
		return nativeResponse(200, map[string]any{})
	})
	a.Config.Fleet = fleet
	err := a.cycle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "owner/first") || strings.Join(asked, ",") != "owner/first,owner/second" {
		t.Fatalf("残りのリポジトリに問い合わせていません: %v %v", asked, err)
	}
}

// ネイティブテストのGit準備の失敗は、どの段かと理由を結果の文に残す。
func TestExecuteNamesFailedGitStep(t *testing.T) {
	a := appleRoundRunner(t, func(*http.Request) *http.Response { return nativeResponse(200, map[string]any{}) })
	a.Config.Git = filepath.Join(t.TempDir(), "missing-git")
	result := a.execute(context.Background(), "owner/project", nativetest.Target{Host: "apple", Head: strings.Repeat("a", 40), Base: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40)}, fleetconfig.Repository{Name: "owner/project"})
	if result.Status != "unrunnable" || !strings.Contains(result.Message, "Git準備（init）") {
		t.Fatalf("失敗した段がありません: %+v", result)
	}
}

// テストの失敗の詳細は、理由の出る末尾を残し、1000文字に収める。
func TestDiagnosticKeepsEnd(t *testing.T) {
	a := &Runner{}
	got := a.diagnostic(process.Result{Err: strings.Repeat("x", 3000) + "the real reason"}, nil)
	if !strings.HasSuffix(got, "the real reason") || len([]rune(got)) != 1000 {
		t.Fatalf("末尾が残っていません: %d", len([]rune(got)))
	}
}

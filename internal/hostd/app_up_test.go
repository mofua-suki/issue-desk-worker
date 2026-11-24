package hostd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/process"
)

type appUpFake struct {
	now     time.Time
	calls   []string
	healthy int // 起動確認が何回目で成功するか（0なら成功しない）
	checks  int
	fail    string // この名前のコマンドを失敗させる
}

func (f *appUpFake) runtime() *Runtime {
	return &Runtime{
		Now:   func() time.Time { return f.now },
		Sleep: func(ctx context.Context, d time.Duration) error { f.now = f.now.Add(d); return nil },
		Run: func(ctx context.Context, name string, args []string, o process.Options) (process.Result, error) {
			f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
			f.now = f.now.Add(time.Second)
			if name == f.fail {
				return process.Result{Code: 1}, nil
			}
			if name == "check" {
				f.checks++
				if f.healthy == 0 || f.checks < f.healthy {
					return process.Result{Code: 1}, nil
				}
			}
			return process.Result{}, nil
		},
	}
}

func appUpFixture(t *testing.T) App {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"compose.yaml", ".env"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return App{Repo: "owner/app", Path: root, ComposeFiles: []string{"compose.yaml"}, EnvFile: ".env",
		Before: [][]string{{"before", "1"}}, After: [][]string{{"after", "2"}}, Health: json.RawMessage(`["check","--ok"]`)}
}

// 配置前のコマンド→compose up→配置後のコマンド→起動確認の順に呼び、起動確認が通るまで繰り返す。
func TestAppUpRunsStepsInOrderAndWaitsForHealth(t *testing.T) {
	a := appUpFixture(t)
	f := &appUpFake{now: time.Unix(1_800_000_000, 0), healthy: 3}
	if err := f.runtime().appUp(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"before 1",
		"docker compose --project-directory " + a.Path + " --env-file " + filepath.Join(a.Path, ".env") + " -f " + filepath.Join(a.Path, "compose.yaml") + " up -d --build",
		"after 2",
		"check --ok", "check --ok", "check --ok",
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("呼び出しが違います:\n%q\n%q", f.calls, want)
	}
}

// 途中のコマンドが失敗したら、その先を呼ばずに失敗を返す。
func TestAppUpStopsAtFailedStep(t *testing.T) {
	a := appUpFixture(t)
	f := &appUpFake{now: time.Unix(1_800_000_000, 0), healthy: 1, fail: "docker"}
	if err := f.runtime().appUp(context.Background(), a); err == nil {
		t.Fatal("compose の失敗を返していません")
	}
	if len(f.calls) != 2 || !strings.HasPrefix(f.calls[1], "docker ") {
		t.Fatalf("失敗の後も呼んでいます: %q", f.calls)
	}
}

// 起動確認が期限（180秒）までに通らなければ失敗を返す。
func TestAppUpFailsWhenHealthNeverPasses(t *testing.T) {
	a := appUpFixture(t)
	start := time.Unix(1_800_000_000, 0)
	f := &appUpFake{now: start}
	err := f.runtime().appUp(context.Background(), a)
	if err == nil || !strings.Contains(err.Error(), "起動を確認できません") {
		t.Fatalf("起動確認の期限切れを返していません: %v", err)
	}
	if f.checks == 0 || f.now.Sub(start) > 200*time.Second {
		t.Fatalf("起動確認の回数・時間が想定外です: %d回, %s", f.checks, f.now.Sub(start))
	}
}

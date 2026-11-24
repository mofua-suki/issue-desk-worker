package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/providers"
)

func unfinishedValidationReport(status string) json.RawMessage {
	var report map[string]any
	_ = json.Unmarshal(implementationFixture(), &report)
	report["status"] = status
	report["tests"] = []any{map[string]any{"command": "go test ./...", "status": "not_run", "detail": "開始後60秒の制限で中断、未完了"}}
	report["unverified"] = []string{"固定テスト待ち", "追加確認未完了"}
	report["unverifiedKinds"] = []string{"fixed_test_by_controller", "other"}
	return encode(report)
}

func TestUnfinishedSelfValidationPipeline(t *testing.T) {
	for _, tc := range []struct {
		path       string
		kind       string
		generation int
		bugfix     bool
	}{
		{path: "initial", kind: "followup"},
		{path: "repair", kind: "followup"},
		{path: "completion", kind: "followup"},
		{path: "repair-completion", kind: "followup"},
		{path: "non-remediation"},
		{path: "remediation", kind: "followup"},
		{path: "generation-only", generation: 1},
		{path: "bugfix-only", bugfix: true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			path := tc.path
			f := newPipeline(t, "approve")
			continuationHead := ""
			if tc.generation > 0 {
				continuationHead = publishContinuationFixture(t, f)
			}
			// 各条件を独立させる。通常の fixture の Issue 本文に bugfix はない。
			var descriptor Descriptor
			if err := json.Unmarshal(f.Job.Raw, &descriptor); err != nil {
				t.Fatal(err)
			}
			descriptor.Kind = tc.kind
			descriptor.Generation = tc.generation
			descriptor.HeadSHA = continuationHead
			if tc.bugfix {
				target, err := url.Parse(f.server.URL)
				if err != nil {
					t.Fatal(err)
				}
				proxy := httputil.NewSingleHostReverseProxy(target)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/owner/project/issues/7" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"number":7,"title":"Implement feature","body":"bugfix","state":"open","user":{"login":"issue-desk-intake"}}`))
						return
					}
					proxy.ServeHTTP(w, r)
				}))
				t.Cleanup(server.Close)
				f.Executor.Gitea, err = gitea.New(server.URL, strings.Repeat("b", 40))
				if err != nil {
					t.Fatal(err)
				}
			}
			f.Job.Raw = encode(descriptor)
			completionAPI(t, f)
			base := f.Executor.Call
			normal, completions := 0, 0
			repair := strings.HasPrefix(path, "repair")
			remediationCase := path == "remediation" || path == "generation-only" || path == "bugfix-only"
			completion := strings.HasSuffix(path, "completion") || remediationCase
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				if req.ReportOnly {
					completions++
					if !req.Policy.ReadOnly || req.Review || req.Session != "implementation-session" || req.Settings.Provider != "codex" {
						t.Fatal("補完の隔離が失われた", req)
					}
					for _, want := range []string{"テスト実行", "禁止", "検証未完了だけ", "tests.status=not_run", "fixed_test_by_controller", "実際に観測した不合格"} {
						if !strings.Contains(req.Prompt, want) {
							t.Fatalf("補完指示不足: %s", want)
						}
					}
					return providers.Answer{Session: req.Session, JSON: unfinishedValidationReport("completed")}, nil
				}
				a, err := base(ctx, req)
				if !req.Review {
					normal++
					for _, want := range []string{"自主検証に根拠のない短い時間制限", "tests.status=not_run", "固定テストはコントローラーが実行"} {
						if !strings.Contains(req.Prompt, want) {
							t.Fatalf("実際の実装呼出しに指示なし: %s", want)
						}
					}
					a = conversationAnswer(t, req, a)
					if repair && normal == 1 {
						if err := os.Remove(filepath.Join(req.Policy.Tree, "feature.txt")); err != nil {
							t.Fatal(err)
						}
					} else {
						a.JSON = unfinishedValidationReport("completed")
						if path == "non-remediation" || remediationCase {
							var report map[string]any
							_ = json.Unmarshal(a.JSON, &report)
							report["prevention"] = map[string]any{"kind": "test", "detail": ""}
							a.JSON = encode(report)
						}
						if completion && !remediationCase {
							var report map[string]any
							_ = json.Unmarshal(a.JSON, &report)
							report["assumptions"] = []any{}
							a.JSON = encode(report)
						}
					}
				}
				return a, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" {
				t.Fatal(state, err)
			}
			if normal != map[bool]int{false: 1, true: 2}[repair] || completions != map[bool]int{false: 0, true: 1}[completion] {
				t.Fatal(normal, completions)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			var tested TestResult
			if err := json.Unmarshal(saved.Test, &tested); err != nil || !tested.Passed {
				t.Fatal("固定テスト未通過", tested, err)
			}
			if !strings.Contains(string(saved.Result), `"status":"not_run"`) || strings.Contains(string(saved.Result), `"status":"passed"`) {
				t.Fatal("検証報告が変化", string(saved.Result))
			}
		})
	}
}

func TestUnfinishedSelfValidationDoesNotPromoteTerminalReports(t *testing.T) {
	for _, repair := range []bool{false, true} {
		for _, kind := range []string{"question", "failed", "prerequisite"} {
			t.Run(map[bool]string{false: "initial", true: "repair"}[repair]+"/"+kind, func(t *testing.T) {
				f := newPipeline(t, "approve")
				completionAPI(t, f)
				base := f.Executor.Call
				normal := 0
				f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
					if req.ReportOnly {
						t.Fatal("非completedを補完してはならない")
					}
					a, err := base(ctx, req)
					if !req.Review {
						normal++
						if repair && normal == 1 {
							if err := os.Remove(filepath.Join(req.Policy.Tree, "feature.txt")); err != nil {
								t.Fatal(err)
							}
							return a, err
						}
						status := kind
						if kind == "prerequisite" {
							status = "failed"
						}
						var report map[string]any
						_ = json.Unmarshal(unfinishedValidationReport(status), &report)
						report["summary"] = "自主検証が時間切れ、実装不足または判断待ち"
						if kind == "prerequisite" {
							report["prerequisiteMissing"] = map[string]any{"reason": "先行成果物不足", "artifacts": []any{map[string]any{"artifact": "仕様", "issue": 1, "repo": "owner/project"}}}
						}
						a.JSON = encode(report)
					}
					return a, err
				}
				state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
				reason := "question"
				if kind == "prerequisite" && !repair {
					reason = "prerequisite_missing"
				}
				if err != nil || state.Status != "held" || state.HoldReason != reason || f.merged {
					t.Fatal(state, err)
				}
				saved, err := f.Executor.Store.Load(1)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(saved.Result), `"status":"`+map[bool]string{true: "question", false: "failed"}[kind == "question"]+`"`) {
					t.Fatal("状態が変化", string(saved.Result))
				}
			})
		}
	}
}

package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestExecutionReportHandoffIndependentWorkers(t *testing.T) {
	for _, mode := range []string{"accepted", "403", "409", "503", "disconnect", "different-generation", "unassigned", "changed-body", "duplicate", "legacy-comment"} {
		t.Run(mode, func(t *testing.T) {
			f, d, a := executionReportFixture(t)
			f.AllowUsage = true
			plan, err := parseExecutionPlan(d)
			if err != nil {
				t.Fatal(err)
			}
			f.Executor.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				return providers.Answer{JSON: encode(Implementation{Status: "completed", Summary: "実際の報告本文"})}, nil
			}
			fence := func(context.Context) error { return nil }
			if err := f.Executor.deliverExecutionReport(t.Context(), d, a, "", "", plan, fence, false); err != nil {
				t.Fatal(err)
			}
			if f.NoticePosts != 1 || len(f.Comments) != 1 {
				t.Fatal(f.NoticePosts, f.Comments)
			}
			actual := f.Comments[0]["body"].(string)
			sum := sha256.Sum256([]byte(actual))
			if mode == "duplicate" {
				f.Comments = append(f.Comments, f.Comments[0])
			}
			if mode == "legacy-comment" {
				f.Comments[0]["body"] = strings.Replace(actual, fmt.Sprintf(":g%d:", a.Generation), ":", 1)
			}
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/api/v1/jobs/1/state" || req.Method != "POST" {
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
					w.WriteHeader(404)
					return
				}
				calls++
				var got struct {
					Epoch            int64  `json:"epoch"`
					Status           string `json:"status"`
					ControllerReport struct {
						BodySHA string `json:"bodySHA"`
						Marker  string `json:"marker"`
						Head    string `json:"head"`
						PlanSHA string `json:"planSHA"`
					} `json:"controllerReport"`
				}
				if json.NewDecoder(req.Body).Decode(&got) != nil || got.Epoch != 4 || got.Status != "running" || got.ControllerReport.BodySHA != hex.EncodeToString(sum[:]) || got.ControllerReport.Head != a.Head || got.ControllerReport.PlanSHA != a.ExecutionPlanSHA || !strings.Contains(actual, got.ControllerReport.Marker) {
					t.Errorf("invalid receipt: %+v", got)
					w.WriteHeader(409)
					return
				}
				// Model the intake's author and actual comment fingerprint checks.
				author := f.Comments[0]["user"].(map[string]any)["login"].(string)
				if mode == "unassigned" {
					author = "outsider"
				}
				serverBody := f.Comments[0]["body"].(string)
				if mode == "changed-body" {
					serverBody += " edited"
				}
				serverSum := sha256.Sum256([]byte(serverBody))
				if author != "worker" || hex.EncodeToString(serverSum[:]) != got.ControllerReport.BodySHA {
					w.WriteHeader(409)
					return
				}
				switch mode {
				case "403":
					w.WriteHeader(403)
				case "409":
					w.WriteHeader(409)
				case "503":
					w.WriteHeader(503)
				case "disconnect":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
				default:
					io.WriteString(w, `{}`)
				}
			}))
			defer server.Close()
			api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			runtime := t.TempDir()
			store, err := journal.Open(runtime)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cfg := f.Executor.Config
			cfg.RuntimeDir = runtime
			cfg.Host = "worker-b"
			cfg.Assignments.WorkerAccounts = map[string]string{"worker-b": "account-b"}
			bGitea, err := gitea.New(f.server.URL, strings.Repeat("c", 40))
			if err != nil {
				t.Fatal(err)
			}
			f.GiteaOverride = func(w http.ResponseWriter, req *http.Request) bool {
				if req.Header.Get("Authorization") != "token "+strings.Repeat("c", 40) {
					t.Error("Bの監視アカウントで取得していません")
					w.WriteHeader(http.StatusForbidden)
					return true
				}
				return false
			}
			b := &Executor{Config: cfg, API: api, Gitea: bGitea, Store: store}
			aiCalls := 0
			b.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				aiCalls++
				return providers.Answer{}, fmt.Errorf("unexpected AI")
			}
			rec := *a
			bTree := filepath.Join(t.TempDir(), "worker-b-tree")
			f.git(f.root, "clone", a.Tree, bTree)
			rec.Tree = bTree
			rec.Epoch = 4
			rec.ExecutionReport = nil
			rec.ExecutionReportResult = nil
			rec.ExecutionReportHead = ""
			if mode == "different-generation" {
				rec.Generation++
				d.Generation++
			}
			state, err := b.executeReportPlan(t.Context(), d, &rec, "", "", plan, fence)
			if mode == "accepted" {
				if err != nil || state.Status != "merged" || !hasJSON(rec.ExecutionReport) || calls != 1 {
					t.Fatal(err, calls, string(rec.ExecutionReport))
				}
				var result Implementation
				if json.Unmarshal(rec.ExecutionReportResult, &result) != nil || result.Summary != "実際の報告本文" {
					t.Fatal(result)
				}
			} else if err != nil || state.Status != "held" || !rec.ExecutionReportPending || hasJSON(rec.ExecutionReport) || (mode != "different-generation" && mode != "duplicate" && mode != "legacy-comment" && calls == 0) {
				t.Fatal(state.Status, err, calls, string(rec.ExecutionReport))
			}
			if f.NoticePosts != 1 || aiCalls != 0 {
				t.Fatal(f.NoticePosts, aiCalls)
			}
			saved, err := store.Load(rec.ID)
			if err != nil || saved == nil || (mode == "accepted" && (!hasJSON(saved.ExecutionReport) || saved.ExecutionReportPending)) || (mode != "accepted" && (hasJSON(saved.ExecutionReport) || !saved.ExecutionReportPending)) {
				t.Fatal("引継ぎの結果がjournalに保持されていません", saved, err)
			}
		})
	}
}

func TestExecutionReportOldAttemptWithoutMarkerRemainsPending(t *testing.T) {
	f, d, r := executionReportFixture(t)
	plan, err := parseExecutionPlan(d)
	if err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf(executionReportMarkerPrefix+"%d:%s:%s -->", r.ID, r.Head, r.ExecutionPlanSHA)
	n := notification{JobID: r.ID, Epoch: r.Epoch, Host: f.Executor.Config.Host, Author: f.Executor.Config.Assignments.WorkerAccounts[f.Executor.Config.Host], Repo: r.Repo, Target: r.Issue, Kind: "comment", Marker: marker, Head: r.Head, Body: "報告\n\n対象: " + r.Head + "\n" + marker, Stage: "attempted"}
	if err := f.Executor.saveNotification(n); err != nil {
		t.Fatal(err)
	}
	calls := 0
	f.Executor.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		calls++
		return providers.Answer{}, nil
	}
	err = f.Executor.deliverExecutionReport(t.Context(), d, r, "", "", plan, func(context.Context) error { return nil }, false)
	if err == nil || hasJSON(r.ExecutionReport) || calls != 0 || f.NoticePosts != 0 {
		t.Fatal(err, calls, f.NoticePosts)
	}
	raw, err := os.ReadFile(f.Executor.notificationFile(n))
	if err != nil || !strings.Contains(string(raw), `"attempted"`) {
		t.Fatal(err, string(raw))
	}
}

func TestExecutionReportSavedReceiptFromDifferentGenerationIsHeld(t *testing.T) {
	f, d, r := executionReportFixture(t)
	plan, err := parseExecutionPlan(d)
	if err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf(executionReportMarkerPrefix+"%d:g%d:%s:%s -->", r.ID, r.Generation, r.Head, r.ExecutionPlanSHA)
	r.ExecutionReport = encode(map[string]any{"marker": marker})
	r.ExecutionReportHead = r.Head
	r.Generation++
	d.Generation++
	state, err := f.Executor.executeReportPlan(t.Context(), d, r, "", "", plan, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || len(state.ControllerReport) != 0 || !r.ExecutionReportPending || f.NoticePosts != 0 {
		t.Fatal(state.Status, err, f.NoticePosts)
	}
	saved, err := f.Executor.Store.Load(r.ID)
	if err != nil || saved == nil || !hasJSON(saved.ExecutionReport) {
		t.Fatal("別世代の証跡が保留中に失われました", err)
	}
	state, err = f.Executor.executeReportPlan(t.Context(), d, saved, "", "", plan, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" || len(state.ControllerReport) != 0 || f.NoticePosts != 0 {
		t.Fatal("再開時に別世代の証跡を使いました", state.Status, err, f.NoticePosts)
	}
}

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func deniedRaw(value any) json.RawMessage { b, _ := json.Marshal(value); return b }
func agyMeasured(denied ...json.RawMessage) providers.Answer {
	zero, one := int64(0), int64(1)
	return providers.Answer{JSON: implementationFixture(), Session: "conv-1", Outcome: "completed", Usage: providers.Usage{Input: &one, Output: &one, Cached: &zero, CacheWrite: &zero}, Denied: denied}
}
func agyHoldFixture(t *testing.T) {
	oldRelease := antigravityReleasePath
	root := t.TempDir()
	antigravityReleasePath = filepath.Join(root, "release.json")
	_ = os.WriteFile(antigravityReleasePath, []byte(`{"version":"1.2.9"}`), 0600)
	t.Cleanup(func() { antigravityReleasePath = oldRelease })
}
func TestAntigravityDenialClassificationMatchesOldCaller(t *testing.T) {
	e := &Executor{}
	e.Config.RuntimeDir = "/fixture/runtime"
	e.Config.WorkerTokenFile = "/fixture/runtime/workspaces/private/token"
	_ = json.Unmarshal([]byte(`{"antigravity":{"settingsFile":"/fixture/gemini/antigravity-cli/settings.json"}}`), &e.Config.Fleet)
	expected := []any{"RunCommand", map[string]any{"tool": "Bash", "command": "Read"}, map[string]any{"tool": "RunCommand", "display_name": "Read"}, map[string]any{"tool": "Edit", "path": "src/a"}, map[string]any{"tool": "Read", "path": "/outside/a"}, map[string]any{"tool": "Read", "path": "../outside/a"}, map[string]any{"tool": "Read", "path": "/fixture/runtime/workspaces-other/a"}, map[string]any{"tool": "Read", "path": "private/a"}, map[string]any{"tool": "Read", "path": 42}}
	for _, v := range expected {
		if got := e.classifyAGYDenials([]json.RawMessage{deniedRaw(v)}); got.Inside+got.Pathless != 0 {
			t.Fatal("想定内を想定外に分類", v, got)
		}
	}
	inside := []any{map[string]any{"tool": "Read", "path": "src/a"}, map[string]any{"tool": "ViewFile", "arguments": map[string]any{"file_path": "./src/a"}}, "Read(/fixture/runtime/workspaces/repo/a)", map[string]any{"tool": "Read", "uri": "file:///fixture/runtime/workspaces/repo/a%20b"}}
	for _, v := range inside {
		if got := e.classifyAGYDenials([]json.RawMessage{deniedRaw(v)}); got.Inside != 1 || got.Pathless != 0 {
			t.Fatal(v, got)
		}
	}
	for _, v := range []any{"Read", "Read denied", map[string]any{"tool": "Read"}, map[string]any{"action": "read_file", "path": "   "}} {
		if got := e.classifyAGYDenials([]json.RawMessage{deniedRaw(v)}); got.Pathless != 1 || got.Inside != 0 {
			t.Fatal(v, got)
		}
	}
}
func TestAntigravityCommandDenialGetsOneSameConversationReminder(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged-fails", true: "file-repair"}[repair], func(t *testing.T) {
			agyHoldFixture(t)
			f := antigravityPipeline(t)
			original := f.Executor.Call
			calls := []string{}
			turn := 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				calls = append(calls, req.Settings.Provider)
				if req.Review {
					return original(ctx, req)
				}
				turn++
				if turn == 2 {
					if req.Session != "conv-1" || !strings.Contains(req.Prompt, "コマンドを使わず") {
						t.Fatal("同会話の念押しではありません", req.Session, req.Prompt)
					}
					if repair {
						_ = os.WriteFile(filepath.Join(req.Policy.Tree, "feature.txt"), []byte("accepted feature\n"), 0600)
						return agyMeasured(), nil
					}
				}
				return agyMeasured(deniedRaw(map[string]any{"tool": "RunCommand", "command": "npm test"})), nil
			}
			out, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if repair {
				if out.Status != "merged" || strings.Join(calls, ",") != "antigravity,antigravity,codex" {
					t.Fatal(out, calls)
				}
			} else {
				if out.Status != "held" || strings.Join(calls, ",") != "antigravity,antigravity" {
					t.Fatal(out, calls)
				}
			}
			r, _ := f.Executor.Store.Load(1)
			if r.ProviderSwitch != nil || len(r.AntigravityDenied) != 2 && !repair {
				t.Fatal("想定内コマンド拒否で切替", r)
			}
		})
	}
}
func TestAntigravityUnexpectedPathlessFailureSwitchesSameTreeAndIndependentReview(t *testing.T) {
	agyHoldFixture(t)
	f := antigravityPipeline(t)
	original := f.Executor.Call
	calls := []string{}
	tree := ""
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		calls = append(calls, req.Settings.Provider)
		if req.Settings.Provider == "antigravity" {
			tree = req.Policy.Tree
			a := agyMeasured(deniedRaw("Read"))
			var result map[string]any
			_ = json.Unmarshal(a.JSON, &result)
			result["status"] = "failed"
			a.JSON = encode(result)
			return a, nil
		}
		if !req.Review && (tree != req.Policy.Tree || req.Session != "") {
			t.Fatal("同tree・別会話を保持しません")
		}
		return original(ctx, req)
	}
	out, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || out.Status != "merged" || strings.Join(calls, ",") != "antigravity,claude,codex" {
		t.Fatal(out, err, calls)
	}
	r, _ := f.Executor.Store.Load(1)
	if r.ProviderSwitch == nil || r.ProviderSwitch.Cause != "denial" || r.AntigravityDenialHold != nil {
		t.Fatal("pathlessで版全体を保留", r)
	}
}
func TestAntigravityDenialQuotaAndTransientNeverSwitch(t *testing.T) {
	for _, quota := range []bool{false, true} {
		t.Run(map[bool]string{false: "transient", true: "quota"}[quota], func(t *testing.T) {
			agyHoldFixture(t)
			f := antigravityPipeline(t, false)
			f.Executor.transientWait = func(context.Context, time.Duration) error { return nil }
			completionAPI(t, f)
			r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Epoch: 3, Provider: "antigravity", Tree: filepath.Join(f.Executor.Config.RuntimeDir, "workspaces", "denied")}
			_ = os.MkdirAll(r.Tree, 0700)
			_ = f.Executor.Store.Save(r)
			d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Provider: "antigravity"}}
			calls := 0
			expected := error(errors.New("transient"))
			if quota {
				expected = &providers.UsageLimit{Provider: "antigravity"}
			}
			f.Executor.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				calls++
				a := agyMeasured(deniedRaw("Read"))
				a.Transient = !quota
				return a, expected
			}
			_, err := f.Executor.callAI(context.Background(), d, r, "fixture", false, nil)
			wantCalls := 3
			if quota {
				wantCalls = 1
			}
			if !errors.Is(err, expected) || calls != wantCalls || r.ProviderSwitch != nil {
				t.Fatal("quota/transientで切替", err, calls, r)
			}
		})
	}
}
func TestAntigravityWorkspaceReadHoldUsesJournalAndVersion(t *testing.T) {
	agyHoldFixture(t)
	f := antigravityPipeline(t, false)
	f.Executor.Config.WorkerTokenFile = filepath.Join(f.root, "private", "worker-token")
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Epoch: 3, Provider: "antigravity", Tree: filepath.Join(f.Executor.Config.RuntimeDir, "workspaces", "denied")}
	_ = os.MkdirAll(r.Tree, 0700)
	_ = f.Executor.Store.Save(r)
	d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Provider: "antigravity"}}
	f.Executor.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		a := agyMeasured(deniedRaw(map[string]any{"tool": "Read", "path": filepath.Join(r.Tree, "src", "a.go")}))
		var result map[string]any
		_ = json.Unmarshal(a.JSON, &result)
		result["status"] = "question"
		a.JSON = encode(result)
		return a, nil
	}
	_, err := f.Executor.callAI(context.Background(), d, r, "fixture", false, nil)
	if err != nil || r.ProviderSwitch != nil || r.AntigravityDenialHold == nil {
		t.Fatal(err, r)
	}
	// A later job in the same journal observes the persisted hold without a colors volume.
	r2 := &journal.Record{ID: 2, Repo: "owner/project", Issue: 7, Epoch: 3, Provider: "antigravity"}
	if err := f.Executor.Store.Save(r2); err != nil {
		t.Fatal(err)
	}
	later := &Executor{Config: f.Executor.Config, API: f.API, Store: f.Executor.Store}
	called := ""
	later.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
		called = req.Settings.Provider
		return agyMeasured(), nil
	}
	_, err = later.callAI(context.Background(), Descriptor{Descriptor: jobspec.Descriptor{ID: 2, Provider: "antigravity"}}, r2, "new job", false, nil)
	if err != nil || called != "claude" || r2.ProviderSwitch.Cause != "denial" {
		t.Fatal("journalの保留を後続ジョブが引き継ぎません", err, called, r2)
	}
	_ = os.WriteFile(antigravityReleasePath, []byte(`{"version":"1.3.0"}`), 0600)
	if hold, err := later.antigravityHold(r2); err != nil || hold != nil {
		t.Fatal("別版へ保留を持ち越しました", hold, err)
	}
}

func TestAntigravityDenialFallbackDoesNotBypassUsageAdmission(t *testing.T) {
	agyHoldFixture(t)
	f := antigravityPipeline(t, false)
	originalAPI := f.API
	checks := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/jobs/1/events" {
			checks++
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"job":{"id":1},"usageLimitReason":"未測定使用量があるため停止します"}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		reply, err := originalAPI.Explicit(r.Context(), r.Method, r.URL.RequestURI(), raw)
		if err != nil {
			t.Error(err)
			w.WriteHeader(502)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.Status)
		w.Write(reply.Body)
	}))
	defer server.Close()
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.API = api
	calls := []string{}
	f.Executor.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
		calls = append(calls, req.Settings.Provider)
		a := agyMeasured(deniedRaw("Read"))
		var body map[string]any
		_ = json.Unmarshal(a.JSON, &body)
		body["status"] = "failed"
		a.JSON = encode(body)
		return a, nil
	}
	out, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || out.Status != "failed" || checks != 1 || strings.Join(calls, ",") != "antigravity" {
		t.Fatal("使用量門を迂回してClaudeを開始しました", out, err, calls, checks)
	}
}
func TestAntigravityDeniedRecordsAreBoundedRedactedAndConflictingHoldInvalid(t *testing.T) {
	agyHoldFixture(t)
	f := antigravityPipeline(t, false)
	f.Executor.secret = "fixture-worker-secret"
	r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7}
	denied := []json.RawMessage{}
	for i := 0; i < 21; i++ {
		denied = append(denied, deniedRaw("Bash fixture-worker-secret idk_"+strings.Repeat("s", 43)+strings.Repeat("x", 400)))
	}
	if _, err := f.Executor.recordAntigravityDenied(Descriptor{Descriptor: jobspec.Descriptor{ID: 1}}, r, denied); err != nil {
		t.Fatal(err)
	}
	record := r.AntigravityDenied[0]
	if record.Count != 21 || len(record.Actions) != 20 {
		t.Fatal(record)
	}
	for _, action := range record.Actions {
		if len([]rune(action)) > 300 || strings.Contains(action, "fixture-worker-secret") || strings.Contains(action, "idk_") {
			t.Fatal("生秘密または上限超過", action)
		}
	}
	var hold journal.ProviderDenialHold
	if err := json.Unmarshal([]byte(`{"version":"1.2.9#r2","at":"2026-11-01T00:00:00Z","job":"owner/repo#1","count":2,"inside":0}`), &hold); err != nil || validAGYHold(&hold, antigravityHoldVersion()) {
		t.Fatal("明示矛盾holdを受理", hold, err)
	}
}

// Records written before inside existed must load, and must not hold the provider.
func TestAntigravityHoldWithoutInsideIsReadableAndIgnored(t *testing.T) {
	var hold journal.ProviderDenialHold
	raw := []byte(`{"version":"v#r2","count":3,"job":"owner/project#1","at":"2026-11-03T00:00:00Z"}`)
	if err := json.Unmarshal(raw, &hold); err != nil {
		t.Fatal(err)
	}
	if validAGYHold(&hold, "v#r2") {
		t.Fatal("a hold without inside was treated as valid")
	}
	hold.Inside = hold.Count
	if !validAGYHold(&hold, "v#r2") {
		t.Fatal("a complete hold was rejected")
	}
}

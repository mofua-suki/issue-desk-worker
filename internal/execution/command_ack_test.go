package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// ackServer は命令を返し、ackの本文を命令IDごとに記録する。
func ackServer(t *testing.T, rows []command, status string) (*Executor, func() map[string]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	acks := map[string]map[string]any{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/commands":
			json.NewEncoder(w).Encode(map[string]any{"commands": rows, "next": 1})
		case r.URL.Path == "/api/v1/jobs/1":
			json.NewEncoder(w).Encode(map[string]any{"job": map[string]any{"id": 1, "status": status, "leaseHost": "fixture", "leaseEpoch": 3}})
		case strings.HasSuffix(r.URL.Path, "/ack"):
			var body struct {
				Result map[string]any `json:"result"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			acks[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/commands/"), "/ack")] = body.Result
			mu.Unlock()
			w.Write([]byte(`{}`))
		default:
			t.Errorf("予期しない経路: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	store, err := journal.Open(filepath.Join(t.TempDir(), "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e := &Executor{API: api, Store: store, Config: Configuration{Host: "fixture"}}
	return e, func() map[string]map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return acks
	}
}

// マージを始めた仕事への停止・追加指示は、無視したことと、追加指示なら新しいIssueが要ることを返す。
func TestCommandAfterMergeStartedIsIgnoredWithReason(t *testing.T) {
	e, acks := ackServer(t, []command{instructionCommand("stop-a", "stop"), instructionCommand("instruction-a", "instruction")}, "running")
	active := &control{epoch: 3, cancel: func() { t.Error("マージ開始後に中断しました") }, mergeStarted: true}
	e.controls.byID = map[int64]*control{1: active}
	if err := e.pollCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := acks()
	if got["stop-a"]["ignored"] != "merge_started" || got["stop-a"]["requiresNewIssue"] != false {
		t.Fatalf("停止の応答: %v", got["stop-a"])
	}
	if got["instruction-a"]["ignored"] != "merge_started" || got["instruction-a"]["requiresNewIssue"] != true {
		t.Fatalf("追加指示の応答: %v", got["instruction-a"])
	}
	if active.reason != "" || len(active.ids) != 0 {
		t.Fatal("マージ開始後の命令を受け付けました")
	}
}

// 仕事が処理した命令のackが届かなかったときは、次の確認で ignored ではなく handled と報告する。
func TestHandledCommandIsReportedHandledAfterLostAck(t *testing.T) {
	e, acks := ackServer(t, []command{instructionCommand("stop-a", "stop")}, "stopped")
	e.commands.handled.add("stop-a")
	if err := e.pollCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := acks()["stop-a"]; got["handled"] != true || got["ignored"] != nil {
		t.Fatalf("処理済みの停止を無視と報告しました: %v", got)
	}
	if e.commands.handled.has("stop-a") {
		t.Fatal("届いた後も処理済みとして残っています")
	}
}

func TestHandledCommandsAreBounded(t *testing.T) {
	var h handledCommands
	for i := 0; i <= handledCommandLimit; i++ {
		h.add(strings.Repeat("x", i+1))
	}
	if h.has("x") || !h.has("xx") || !h.has(strings.Repeat("x", handledCommandLimit+1)) {
		t.Fatal("上限を超えた古い命令から忘れていません")
	}
}

// 仕事が停止を反映した後にackが届かなければ、処理済みとして覚え、届けば忘れる。
func TestExecuteRemembersStopWhoseAckFailed(t *testing.T) {
	for _, status := range []int{503, 0} {
		f := newPipeline(t, "approve")
		f.AckStatus = status
		stopped := false
		_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error {
			if !stopped {
				stopped = true
				active := f.Executor.controls.get(f.Job.ID)
				active.mu.Lock()
				active.reason, active.ids = "stop", []string{"stop-x"}
				active.mu.Unlock()
				active.cancel()
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := f.Executor.commands.handled.has("stop-x"); got != (status != 0) {
			t.Fatalf("ack %d: 処理済みの記録が %t です", status, got)
		}
	}
}

func TestStopBeforeFirstImplementationResumes(t *testing.T) {
	for _, scenario := range []string{"resume", "old_record", "started", "artifact"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.Job.LeaseEpoch = 1
			f.CurrentLeaseEpoch = 1
			f.GiteaOverride = func(w http.ResponseWriter, r *http.Request) bool {
				if r.URL.Path != "/api/v1/repos/owner/project" {
					return false
				}
				active := f.Executor.controls.get(f.Job.ID)
				active.mu.Lock()
				active.reason = "stop"
				active.mu.Unlock()
				active.cancel()
				w.Write([]byte(`{"default_branch":"main","empty":false,"permissions":{"push":true}}`))
				return true
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "stopped" {
				t.Fatalf("stop: %+v %v", state, err)
			}
			if len(f.Calls) != 0 {
				t.Fatalf("implementation started before stop: %v", f.Calls)
			}
			if err := f.Executor.API.State(context.Background(), f.Job.ID, 1, state); err != nil {
				t.Fatal(err)
			}
			if err := f.Executor.Reported(f.Job.ID, 1); err != nil {
				t.Fatal(err)
			}
			record, err := f.Executor.Store.Load(f.Job.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "old_record":
				record.StoppedBeforeStart = false
			case "started":
				record.Runs = 1
				record.StartedGenerations = []int{0}
			case "artifact":
				record.Head = strings.Repeat("a", 40)
			}
			if err := f.Executor.Store.Save(record); err != nil {
				t.Fatal(err)
			}
			var descriptor map[string]any
			if err := json.Unmarshal(f.Job.Raw, &descriptor); err != nil {
				t.Fatal(err)
			}
			var view map[string]json.RawMessage
			if err := json.Unmarshal(state.View, &view); err != nil {
				t.Fatal(err)
			}
			descriptor["view"] = view
			f.Job.Raw = encode(descriptor)
			f.Job.LeaseEpoch = 2
			f.CurrentLeaseEpoch = 2
			f.CurrentView = view
			f.GiteaOverride = nil
			state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "resume" {
				count := 0
				for _, call := range f.Calls {
					if call == "codex" {
						count++
					}
				}
				if count != 1 || state.Status == "held" {
					t.Fatalf("resume: status=%s calls=%v result=%s", state.Status, f.Calls, state.Result)
				}
			} else if state.Status != "held" || len(f.Calls) != 0 {
				t.Fatalf("continuation protection: status=%s calls=%v result=%s", state.Status, f.Calls, state.Result)
			}
			stopped := 0
			for _, reported := range f.ReceivedStates {
				if reported.Status == "stopped" {
					stopped++
				}
			}
			if stopped != 1 {
				t.Fatalf("stopped reports: %d", stopped)
			}
		})
	}
}

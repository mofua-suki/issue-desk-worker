package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/journal"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func instructionCommand(id, kind string) command {
	c := command{ID: id, Kind: kind}
	c.Payload.JobID = 1
	c.Payload.Epoch = 3
	if kind == "instruction" {
		c.Payload.Text = "追加本文"
		c.Payload.Context = "追加根拠"
	}
	return c
}
func commandExecutor(t *testing.T, rows []command, running bool) (*Executor, *int) {
	t.Helper()
	dir := t.TempDir()
	store, err := journal.Open(filepath.Join(dir, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	acks := 0
	var e *Executor
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/commands":
			json.NewEncoder(w).Encode(map[string]any{"commands": rows, "next": 1})
		case r.URL.Path == "/api/v1/jobs/1":
			status := "queued"
			if running {
				status = "running"
			}
			json.NewEncoder(w).Encode(map[string]any{"job": map[string]any{"id": 1, "status": status, "leaseHost": "fixture", "leaseEpoch": 3}})
		case strings.HasSuffix(r.URL.Path, "/ack"):
			acks++
			if strings.Contains(r.URL.Path, "instruction") {
				d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1}}
				if err := e.attachInstructions(&d); err != nil || !strings.Contains(string(d.View["instructions"]), "追加本文") {
					t.Error("指示のdurable保存前にackしました")
				}
			}
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
	e = &Executor{API: api, Store: store, Config: Configuration{Host: "fixture", Fleet: Fleet{RuntimeDir: filepath.Join(dir, "runtime")}}}
	if err := store.Save(&journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Epoch: 3, Stage: "implement"}); err != nil {
		t.Fatal(err)
	}
	return e, &acks
}
func TestAssignmentRegistrationWindowDoesNotConsumeCommands(t *testing.T) {
	for _, kind := range []string{"stop", "instruction"} {
		t.Run(kind, func(t *testing.T) {
			e, acks := commandExecutor(t, []command{instructionCommand(kind, kind)}, true)
			if err := e.pollCommands(context.Background()); err != nil {
				t.Fatal(err)
			}
			if *acks != 0 {
				t.Fatal("Execute登録前の有効指示を消費しました")
			}
			e.controls.byID = map[int64]*control{1: {epoch: 3, cancel: func() {}}}
			if err := e.pollCommands(context.Background()); err != nil {
				t.Fatal(err)
			}
			if e.controls.byID[1].reason != kind || *acks != 0 {
				t.Fatal("登録後の指示を保持しませんでした")
			}
		})
	}
}
func TestInstructionIsDurableDeduplicatedAndStopTakesPriority(t *testing.T) {
	for _, order := range [][]command{{instructionCommand("instruction-a", "instruction"), instructionCommand("stop-a", "stop")}, {instructionCommand("stop-a", "stop"), instructionCommand("instruction-a", "instruction")}} {
		e, _ := commandExecutor(t, order, true)
		e.controls.byID = map[int64]*control{1: {epoch: 3, cancel: func() {}}}
		for i := 0; i < 2; i++ {
			if err := e.pollCommands(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if e.controls.byID[1].reason != "stop" || len(e.controls.byID[1].ids) != 2 {
			t.Fatal("停止優先・同ID重複排除が不正です")
		}
		restarted := &Executor{Config: e.Config}
		d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, View: map[string]json.RawMessage{"instructions": encode([]map[string]any{{"id": "command:instruction-a", "text": "追加本文", "context": "追加根拠"}})}}}
		if err := restarted.attachInstructions(&d); err != nil {
			t.Fatal(err)
		}
		prompt := implementationPrompt(d, "題", "本文", &journal.Record{})
		if strings.Count(prompt, "追加本文") != 1 || !strings.Contains(prompt, "追加根拠") {
			t.Fatal("再起動/Server正本とspoolで指示が消失・重複しました")
		}
		other := &Executor{Config: Configuration{Fleet: Fleet{RuntimeDir: t.TempDir()}}}
		if err := other.attachInstructions(&d); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(reviewPrompt(d, "題", "本文", &journal.Record{}, gitwork.Snapshot{}, "diff"), "追加本文") {
			t.Fatal("別hostの新Descriptorからレビューへ指示が渡りません")
		}
	}
}
func TestInactiveOldLeaseSavesInstructionBeforeAck(t *testing.T) {
	e, acks := commandExecutor(t, []command{instructionCommand("instruction-a", "instruction")}, false)
	if err := e.pollCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
	if *acks != 1 {
		t.Fatal("保存した旧lease指示をackしませんでした")
	}
}

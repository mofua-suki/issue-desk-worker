package execution

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

func TestReviewNotesDeliveryAndDeduplication(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, b)
		if len(bodies) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
	defer server.Close()
	api, err := intake.New(config.Bootstrap{ServerURL: server.URL, ServerTokenFile: "test", WorkerID: "test"}, "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{API: api, Store: store, secret: "test-token"}
	r := &journal.Record{ID: 1, Repo: "owner/repo", Issue: 2, Epoch: 3, Head: "head", Review: encode(map[string]any{"findings": []string{"test-token detail", "two"}, "findingKinds": []string{"security", "other"}, "followups": []string{"follow"}, "improvements": []string{"improve"}})}
	fences := 0
	fence := func(context.Context) error { fences++; return nil }
	if err := e.sendReviewNotes(context.Background(), r, fence, false); err != nil {
		t.Fatal(err)
	}
	if err := e.sendReviewNotes(context.Background(), r, fence, false); err != nil {
		t.Fatal(err)
	}
	if err := e.sendReviewNotes(context.Background(), r, fence, true); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 4 || len(saved.NotesAttempted) != 4 || fences != 2 {
		t.Fatalf("bodies=%v saved=%v fences=%d", bodies, saved.NotesAttempted, fences)
	}
	if bodies[0]["kind"] != "finding" || bodies[2]["kind"] != "followup" || bodies[3]["kind"] != "improvement" {
		t.Fatal(bodies)
	}
	data := bodies[0]["data"].(map[string]any)
	if strings.Contains(data["text"].(string), "test-token") || data["findingKind"] != "security" {
		t.Fatal(data)
	}
}

func TestFinishMergedSavesBeforeNotesFence(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Executor{Store: store}
	r := &journal.Record{ID: 1, Issue: 2, Review: encode(map[string]any{"followups": []string{"follow"}})}
	state, err := e.finishMerged(context.Background(), r, func(context.Context) error { return context.Canceled }, "done", nil)
	if err != nil || state.Status != "merged" {
		t.Fatal(state, err)
	}
	saved, err := store.Load(1)
	if err != nil || saved.Stage != "done" || len(saved.FinalState) == 0 {
		t.Fatal(saved, err)
	}
}

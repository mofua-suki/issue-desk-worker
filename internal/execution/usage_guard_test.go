package execution

import (
	"context"
	"encoding/json"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/journal"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUsageGuardUsesServerDecisionAndAdmissionOnce(t *testing.T) {
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var bodies []map[string]json.RawMessage
	reason := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/jobs/4/events" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body struct {
			Epoch int64                      `json:"epoch"`
			Kind  string                     `json:"kind"`
			Data  map[string]json.RawMessage `json:"data"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Epoch != 8 || body.Kind != "implementation_usage_check" {
			t.Error("epoch/event mismatch")
		}
		bodies = append(bodies, body.Data)
		if reason {
			w.Write([]byte(`{"job":{"id":4},"usageLimitReason":"missing"}`))
		} else {
			w.Write([]byte(`{"job":{"id":4},"usageLimitReason":null}`))
		}
	}))
	defer server.Close()
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{API: api, Store: store}
	r := &journal.Record{ID: 4, Epoch: 8, Generation: 2, Runs: 1}
	d := Descriptor{Descriptor: jobspec.Descriptor{ID: 4, Generation: 2, View: map[string]json.RawMessage{"reimplementExpected": json.RawMessage(`{"generation":2,"usageAdmission":true}`)}}}
	if err := e.checkImplementationUsage(context.Background(), d, r, false, false); err != nil {
		t.Fatal(err)
	}
	if r.AdmissionID == "" {
		t.Fatal("admission not persisted")
	}
	saved, _ := store.Load(4)
	if saved.AdmissionID != r.AdmissionID {
		t.Fatal("admission mismatch")
	}
	r.Runs++
	if err := e.checkImplementationUsage(context.Background(), d, r, true, true); err != nil {
		t.Fatal(err)
	}
	if string(bodies[0]["admissionId"]) != string(bodies[1]["admissionId"]) || string(bodies[1]["admissionRecall"]) != "true" {
		t.Fatal("recall changed admission")
	}
	reason = true
	if e.checkImplementationUsage(context.Background(), d, r, true, true) == nil {
		t.Fatal("server missing usage accepted")
	}
	if _, ok := bodies[2]["admissionId"]; ok {
		t.Fatal("admission reused")
	}
	r.Usage = []json.RawMessage{json.RawMessage(`{"value":"` + strings.Repeat("x", 100001) + `"}`)}
	_ = e.checkImplementationUsage(context.Background(), d, r, true, false)
	if string(bodies[3]["skipped"]) != "1" || string(bodies[3]["rows"]) != "[]" {
		t.Fatal("oversize treated as known partial usage")
	}
}

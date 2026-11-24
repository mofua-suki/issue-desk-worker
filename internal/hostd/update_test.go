package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/process"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type updateFixture struct {
	Runtime               *Runtime
	Now                   time.Time
	Target                Target
	Drain                 *Drain
	Events                []string
	Starts, Restores      int
	FailStart, FailReport bool
	WrongRegistry         bool
	BusyAfterStart        bool
	Command               *HostCommand
	OnSnapshot            func(*imageSnapshot)
	CommandResults        []CommandResult
	Cycles                []map[string]any
	ResultStatus          int
	DrainStatus           int
	Reported              []string
}

func newUpdateFixture(t *testing.T) *updateFixture {
	t.Helper()
	home := t.TempDir()
	client := filepath.Join(home, "client")
	if err := os.Mkdir(client, 0700); err != nil {
		t.Fatal(err)
	}
	f := &updateFixture{Now: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), Target: Target{Tag: strings.Repeat("b", 12), Revision: strings.Repeat("b", 40), Registry: "registry.example:5443", Digest: "sha256:" + strings.Repeat("d", 64)}}
	writeEnv := func(tag string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(client, ".env"), []byte("FLAVOR="+f.Target.Flavor+"\nISSUE_DESK_CLIENT_TAG="+tag+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeEnv(strings.Repeat("a", 12))
	if err := os.WriteFile(filepath.Join(home, "registry-password"), []byte("synthetic-registry-password"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer idh_"+strings.Repeat("a", 43) {
			t.Error("hostd authentication mismatch")
		}
		f.Events = append(f.Events, req.Method+" "+req.URL.Path)
		switch {
		case strings.HasSuffix(req.URL.Path, "desired-image"):
			zero := 0
			h := &Heartbeat{At: f.Now, Implementing: &zero, Publishing: &zero, Revision: strings.Repeat("a", 40), Ready: true}
			if f.Drain != nil {
				h.Drain.Hostd = f.Drain
			}
			if f.Starts > 0 && !f.FailStart {
				h.Revision = f.Target.Revision
				if f.BusyAfterStart {
					one := 1
					h.Implementing = &one
					h.Ready = false
				}
			}
			snapshot := imageSnapshot{Desired: &f.Target, Heartbeat: h, Command: f.Command}
			if f.OnSnapshot != nil {
				f.OnSnapshot(&snapshot)
			}
			json.NewEncoder(w).Encode(snapshot)
		case strings.HasSuffix(req.URL.Path, "/drain") && req.Method == "PUT":
			if f.DrainStatus != 0 {
				w.WriteHeader(f.DrainStatus)
				return
			}
			var body struct {
				Until time.Time `json:"until"`
			}
			json.NewDecoder(req.Body).Decode(&body)
			f.Drain = &Drain{By: "hostd:test", At: f.Now, Until: body.Until}
			json.NewEncoder(w).Encode(map[string]any{"drain": f.Drain})
		case strings.HasSuffix(req.URL.Path, "/drain") && req.Method == "DELETE":
			f.Drain = nil
			w.Write([]byte("{}"))
		case strings.HasSuffix(req.URL.Path, "image-result"):
			var body struct {
				Status string `json:"status"`
			}
			json.NewDecoder(req.Body).Decode(&body)
			if f.FailReport {
				f.FailReport = false
				w.WriteHeader(503)
				return
			}
			f.Reported = append(f.Reported, body.Status)
			w.Write([]byte("{}"))
		case strings.HasSuffix(req.URL.Path, "hostd-health"):
			var body struct {
				Result *CommandResult `json:"commandResult"`
				Cycle  map[string]any `json:"cycle"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Result != nil {
				f.CommandResults = append(f.CommandResults, *body.Result)
				if f.ResultStatus != 0 {
					w.WriteHeader(f.ResultStatus)
					return
				}
			}
			if body.Cycle != nil {
				f.Cycles = append(f.Cycles, body.Cycle)
			}
			w.Write([]byte(`{"saved":true}`))
		default:
			t.Error("unexpected endpoint", req.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	r := &Runtime{Config: Configuration{URL: server.URL, Host: "test", ClientDir: client, Home: home, WaitMinutes: 120, ReadySeconds: 10}, HTTP: server.Client(), token: "idh_" + strings.Repeat("a", 43), Now: func() time.Time { return f.Now }, Sleep: func(ctx context.Context, d time.Duration) error { f.Now = f.Now.Add(d); return nil }}
	f.Runtime = r
	r.Run = func(ctx context.Context, name string, args []string, options process.Options) (process.Result, error) {
		f.Events = append(f.Events, name+" "+strings.Join(args, " "))
		if name == "bash" {
			choice := args[len(args)-1]
			if choice == "--mark-green-stopped" {
				t.Error("obsolete green preparation called")
				return process.Result{Code: 1}, nil
			}
			if choice == "--restore-previous" {
				f.Restores++
				writeEnv(strings.Repeat("a", 12))
				return process.Result{}, nil
			}
			f.Starts++
			var persisted *UpdateState
			if err := r.load("state.json", &persisted); err != nil || persisted == nil || persisted.Phase != "updating" {
				t.Error("effect before durable update state", err)
			}
			if f.FailStart {
				writeEnv(choice)
				return process.Result{Code: 1}, nil
			}
			writeEnv(choice)
			f.Now = f.Now.Add(time.Second)
			return process.Result{}, nil
		}
		if name == "docker" {
			image := f.Target.Registry + "/issue-desk-client" + f.Target.Flavor + ":" + f.Target.Tag
			if f.WrongRegistry && len(args) > 0 && args[0] == "compose" {
				image = "127.0.0.1:5000/issue-desk-client:" + f.Target.Tag
			}
			if len(args) > 0 && args[0] == "compose" {
				return process.Result{Out: fmt.Sprintf(`{"services":{"issue-desk-client":{"image":%q}}}`, image)}, nil
			}
			return process.Result{Out: fmt.Sprintf(`[%q]`, f.Target.Registry+"/issue-desk-client"+f.Target.Flavor+"@"+f.Target.Digest)}, nil
		}
		return process.Result{}, errors.New("unexpected command")
	}
	return f
}
func TestDeliveredCommandDoesNotBlockChangedDesiredWhileWaiting(t *testing.T) {
	f := newUpdateFixture(t)
	r := f.Runtime
	f.Command = &HostCommand{ID: "old-command", Kind: "unsupported", ExpiresAt: f.Now.Add(time.Hour)}
	f.Target.Kind = "lazy"
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.CommandResults) != 1 {
		t.Fatal("command result was not delivered")
	}
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	var state *UpdateState
	if err := r.load("state.json", &state); err != nil || state == nil || state.Phase != "waiting" || state.Target.Tag != f.Target.Tag {
		t.Fatalf("initial desired is not waiting: state=%+v err=%v", state, err)
	}
	f.Target.Tag = strings.Repeat("c", 12)
	f.Target.Revision = strings.Repeat("c", 40)
	f.Target.Kind = ""
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.load("state.json", &state); err != nil || state == nil || state.Target.Tag != f.Target.Tag || !state.DrainSet {
		t.Fatalf("changed desired did not advance to drain: state=%+v err=%v", state, err)
	}
	f.Now = f.Now.Add(time.Minute)
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Starts != 1 || len(f.Reported) != 1 || f.Reported[0] != "success" || len(f.CommandResults) != 1 {
		t.Fatalf("update or command result repeated: starts=%d reported=%v command results=%v", f.Starts, f.Reported, f.CommandResults)
	}
}

func TestImageUpdatePersistsDrainAndRetriesReportWithoutRepeatingUpdate(t *testing.T) {
	f := newUpdateFixture(t)
	r := f.Runtime
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Drain == nil || f.Starts != 0 {
		t.Fatal("updated before fresh drained heartbeat")
	}
	f.Now = f.Now.Add(time.Minute)
	f.FailReport = true
	if r.updateCycle(context.Background()) == nil {
		t.Fatal("report error ignored")
	}
	var state *UpdateState
	if err := r.load("state.json", &state); err != nil || state == nil || state.Phase != "reporting" {
		t.Fatal("report state lost", err)
	}
	if f.Starts != 1 || f.Restores != 0 {
		t.Fatalf("report failure caused rollback: starts=%d restores=%d events=%v", f.Starts, f.Restores, f.Events)
	}
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Starts != 1 || f.Restores != 0 || f.Drain != nil || len(f.Reported) != 1 || f.Reported[0] != "success" {
		t.Fatal("update repeated or result wrong")
	}
}
func TestFailedUpdateRestoresPreviousAndReportsFailure(t *testing.T) {
	f := newUpdateFixture(t)
	r := f.Runtime
	f.FailStart = true
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Now = f.Now.Add(time.Minute)
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Starts != 1 || f.Restores != 1 || len(f.Reported) != 1 || f.Reported[0] != "failed" || f.Drain != nil {
		t.Fatalf("failure recovery: starts=%d restores=%d reported=%v drain=%v", f.Starts, f.Restores, f.Reported, f.Drain)
	}
}
func TestRegistryMismatchIsReportedAsDigestMismatchNotReadyTimeout(t *testing.T) {
	f := newUpdateFixture(t)
	r := f.Runtime
	f.WrongRegistry = true
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Now = f.Now.Add(time.Minute)
	started := f.Now
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.failure == nil || r.failure.Detail != "digest_mismatch" || r.failure.Stage != "swap-start" {
		t.Fatalf("failure = %+v, want digest_mismatch", r.failure)
	}
	if f.Restores != 1 || len(f.Reported) != 1 || f.Reported[0] != "failed" {
		t.Fatalf("restores=%d reported=%v", f.Restores, f.Reported)
	}
	if waited := f.Now.Sub(started); waited >= time.Duration(r.Config.ReadySeconds)*time.Second {
		t.Fatalf("waited %v for ready although the image did not match", waited)
	}
}
func TestInterruptedUpdateOnlyRecovers(t *testing.T) {
	f := newUpdateFixture(t)
	r := f.Runtime
	if err := r.save("state.json", &UpdateState{Phase: "updating", Target: f.Target}); err != nil {
		t.Fatal(err)
	}
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Starts != 0 || f.Restores != 1 {
		t.Fatal("interrupted update started again")
	}
}
func TestDrainIgnoresOldOrFutureSamplesAndStopsOnNewWork(t *testing.T) {
	now := time.Now()
	zero, one, two := 0, 1, 2
	before := &Heartbeat{At: now.Add(-time.Second), Implementing: &one, Publishing: &zero}
	for _, at := range []time.Time{now.Add(-time.Hour), now.Add(time.Second)} {
		h := &Heartbeat{At: at, Implementing: &zero, Publishing: &zero}
		if drainDecision(h, before, now.Add(-time.Minute), now, now.Add(time.Hour)) != "wait" {
			t.Fatal("invalid idle sample accepted")
		}
	}
	h := &Heartbeat{At: now, Implementing: &two, Publishing: &zero}
	if drainDecision(h, before, now.Add(-time.Minute), now, now.Add(time.Hour)) != "new_implementation" {
		t.Fatal("new work ignored")
	}
}
func TestNumericUpdateStateRemainsReadable(t *testing.T) {
	var state UpdateState
	if json.Unmarshal([]byte(`{"phase":"waiting","started":1000,"deadline":2000,"until":3000,"greenPreparedAt":4000}`), &state) != nil || state.Started.UnixMilli() != 1000 {
		t.Fatal("numeric state not read")
	}
	raw, err := json.Marshal(state)
	if err != nil || !strings.Contains(string(raw), `"started":"1970-01-01T00:00:01Z"`) || strings.Contains(string(raw), "lazySince") {
		t.Fatal("state time is not RFC3339 or an unset time was written", err, string(raw))
	}
	var again UpdateState
	if err := json.Unmarshal(raw, &again); err != nil || !again.Started.Equal(state.Started) {
		t.Fatal("rewritten state not read", err)
	}
}

func TestUpdateWaitsForBusyWorkerAfterTemporaryAcquisitionFailure(t *testing.T) {
	f := newUpdateFixture(t)
	r := f.Runtime
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Now = f.Now.Add(time.Minute)
	f.BusyAfterStart = true
	r.Sleep = func(ctx context.Context, d time.Duration) error {
		f.Now = f.Now.Add(d)
		if f.Now.Sub(f.Drain.At) > 90*time.Second {
			f.BusyAfterStart = false
		}
		return nil
	}
	if err := r.updateCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Restores != 0 || len(f.Reported) != 1 || f.Reported[0] != "success" {
		t.Fatalf("restores=%d reports=%v", f.Restores, f.Reported)
	}
}

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"issue-desk-worker/internal/providers"
)

func noDiffInterrupted(t *testing.T) (*pipelineFixture, *int, *int, *string) {
	t.Helper()
	f := newPipeline(t, "approve")
	f.Executor.Config.Repositories[0].TestCommand = fixtureShellCommand(f, "true")
	base := f.Executor.Call
	calls := 0
	patches := 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		calls++
		a, err := base(ctx, req)
		if !req.Review {
			if err := os.Remove(filepath.Join(req.Policy.Tree, "feature.txt")); err != nil {
				t.Fatal(err)
			}
		}
		return a, err
	}
	events := `[{"id":10,"type":"comment","user":{"login":"worker"}}]`
	f.GiteaOverride = func(w http.ResponseWriter, r *http.Request) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/repos/owner/project/issues/7/timeline":
			if events == "failure" {
				w.WriteHeader(http.StatusInternalServerError)
				return true
			}
			fmt.Fprint(w, events)
			return true
		case "/api/v1/repos/owner/project/issues/7":
			if r.Method == "PATCH" {
				patches++
				f.closed = true
				events = `[{"id":10,"type":"comment","user":{"login":"worker"}},{"id":11,"type":"close","user":{"login":"worker"}}]`
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Fatal(err)
				}
				conn.Close()
				return true
			}
			if f.closed {
				fmt.Fprint(w, `{"number":7,"title":"Implement feature","body":"受入条件: feature.txtを生成する","state":"closed"}`)
			} else {
				fmt.Fprint(w, `{"number":7,"title":"Implement feature","body":"受入条件: feature.txtを生成する","state":"open","user":{"login":"issue-desk-intake"}}`)
			}
			return true
		}
		return false
	}
	_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if err == nil || !closed {
		t.Fatalf("close was not interrupted: %v", err)
	}
	r, err := f.Executor.Store.Load(1)
	if err != nil || r.NoDiffClose == nil {
		t.Fatalf("missing close evidence: %+v %v", r, err)
	}
	f.CurrentLeaseEpoch = 4
	f.Job.LeaseEpoch = 4
	return f, &calls, &patches, &events
}

func TestNoDiffCloseRecovery(t *testing.T) {
	f, calls, patches, _ := noDiffInterrupted(t)
	before := *calls
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	f.mu.Lock()
	patchCount := *patches
	f.mu.Unlock()
	if err != nil || state.Status != "merged" || *calls != before || patchCount != 1 || f.CreatePosts != 0 {
		t.Fatalf("state=%+v calls=%d/%d PR=%d err=%v", state, *calls, before, f.CreatePosts, err)
	}
}

func TestNoDiffCloseRecoveryGuards(t *testing.T) {
	cases := []string{"evidence", "generation", "target", "account", "lease", "missing", "missing-boundary", "null", "failure", "multiple", "reopen", "empty-account"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			f, calls, patches, events := noDiffInterrupted(t)
			before := *calls
			r, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			switch name {
			case "evidence":
				r.NoDiffClose = nil
			case "generation":
				r.Generation++
			case "target":
				r.Issue++
			case "account":
				*events = `[{"id":10,"type":"comment","user":{"login":"worker"}},{"id":11,"type":"close","user":{"login":"other"}}]`
			case "missing":
				*events = `[]`
			case "missing-boundary":
				*events = `[{"id":11,"type":"close","user":{"login":"worker"}}]`
			case "null":
				*events = `null`
			case "failure":
				*events = "failure"
			case "multiple":
				*events = `[{"id":10,"type":"comment","user":{"login":"worker"}},{"id":11,"type":"close","user":{"login":"worker"}},{"id":12,"type":"close","user":{"login":"worker"}}]`
			case "reopen":
				*events = `[{"id":10,"type":"comment","user":{"login":"worker"}},{"id":11,"type":"close","user":{"login":"worker"}},{"id":12,"type":"reopen","user":{"login":"worker"}}]`
			case "empty-account":
				f.Executor.Config.Assignments.WorkerAccounts[f.Executor.Config.Host] = ""
			}
			f.mu.Unlock()
			if err := f.Executor.Store.Save(r); err != nil {
				t.Fatal(err)
			}
			fence := func(context.Context) error {
				if name == "lease" {
					return errors.New("lease lost")
				}
				return nil
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, fence)
			if name == "lease" || name == "target" {
				if err == nil {
					t.Fatalf("expected rejection: %+v", state)
				}
			} else if err != nil || state.Status != "stopped" {
				t.Fatalf("expected closed stop: %+v, %v", state, err)
			}
			f.mu.Lock()
			patchCount := *patches
			f.mu.Unlock()
			if state.Status == "merged" || *calls != before || patchCount != 1 || f.CreatePosts != 0 {
				b, _ := json.Marshal(state)
				t.Fatalf("state=%s calls=%d err=%v", b, *calls, err)
			}
		})
	}
}

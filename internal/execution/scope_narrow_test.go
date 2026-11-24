package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/jobspec"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestNarrowScopeContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantError bool
		wantScope []string
	}{
		{"granted", 200, `{"result":"granted","job":{"id":1,"scope":["feature.txt"]}}`, false, []string{"feature.txt"}},
		{"empty scope", 200, `{"result":"granted","job":{"id":1,"scope":[]}}`, false, []string{"*"}},
		{"missing path", 200, `{"result":"granted","job":{"id":1,"scope":["other"]}}`, false, []string{"*"}},
		{"unsupported", 400, `{"code":"invalid"}`, false, []string{"*"}},
		{"lease lost", 409, `{"code":"lease_lost"}`, true, []string{"*"}},
		{"wrong job", 200, `{"result":"granted","job":{"id":2}}`, true, []string{"*"}},
		{"not granted", 200, `{"result":"wait"}`, false, []string{"*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPipeline(t, "approve")
			calls := 0
			fences := 0
			f.ScopeActual = func(w http.ResponseWriter, paths []string) {
				calls++
				if !reflect.DeepEqual(paths, []string{"feature.txt"}) {
					t.Errorf("actual=%v", paths)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}
			d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Scope: []string{"*"}}}
			seen := map[string]bool{}
			r := &journal.Record{ID: 1}
			err := f.Executor.narrowScope(context.Background(), &d, 3, []string{"feature.txt"}, func(context.Context) error { fences++; return nil }, func(context.Context) error { return nil }, r, seen)
			if (err != nil) != tc.wantError || !reflect.DeepEqual(d.Scope, tc.wantScope) || calls != 1 || fences != 2 {
				t.Fatalf("err=%v scope=%v calls=%d fences=%d", err, d.Scope, calls, fences)
			}
			if tc.name == "lease lost" {
				var ae *intake.APIError
				if !errors.As(err, &ae) || !ae.LeaseLost() {
					t.Fatal(err)
				}
			}
			if !tc.wantError {
				_ = f.Executor.narrowScope(context.Background(), &d, 3, []string{"feature.txt"}, func(context.Context) error { fences++; return nil }, func(context.Context) error { return nil }, r, seen)
				if calls != 1 {
					t.Fatal("重複送信", calls)
				}
			}
		})
	}
}

func TestNarrowScopeSkipsAndCancellation(t *testing.T) {
	f := newPipeline(t, "approve")
	calls := 0
	f.ScopeActual = func(w http.ResponseWriter, paths []string) {
		calls++
		fmt.Fprint(w, `{"result":"granted","job":{"id":1,"scope":["feature.txt"]}}`)
	}
	d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Scope: []string{"feature.txt"}}}
	seen := map[string]bool{}
	r := &journal.Record{ID: 1}
	fence := func(context.Context) error { return nil }
	for _, paths := range [][]string{nil, {"feature.txt"}, make([]string, 201)} {
		if err := f.Executor.narrowScope(context.Background(), &d, 3, paths, fence, func(context.Context) error { return nil }, r, seen); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal(calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.Scope = []string{"*"}
	if err := f.Executor.narrowScope(ctx, &d, 3, []string{"feature.txt"}, fence, func(context.Context) error { return nil }, r, seen); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestNarrowScopePipelineContinuesAfter400(t *testing.T) {
	f := newPipeline(t, "approve")
	calls := 0
	f.ScopeActual = func(w http.ResponseWriter, paths []string) {
		calls++
		w.WriteHeader(400)
		fmt.Fprint(w, `{"code":"invalid"}`)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || calls != 1 || strings.Join(f.Calls, ",") != "codex,claude" {
		t.Fatalf("state=%+v err=%v calls=%d providers=%v", state, err, calls, f.Calls)
	}
}

func TestNarrowScopeReworkWidensRemovedPath(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	var events []string
	f.ScopeActual = func(w http.ResponseWriter, paths []string) {
		events = append(events, "actual:"+strings.Join(paths, ","))
		json.NewEncoder(w).Encode(map[string]any{"result": "granted", "job": map[string]any{"id": 1, "scope": paths}})
	}
	f.ScopeAdd = func(w http.ResponseWriter, paths []string) {
		events = append(events, "add:"+strings.Join(paths, ","))
		fmt.Fprint(w, `{"result":"granted","job":{"id":1,"scope":["feature.txt","README.md"]}}`)
	}
	original := f.Executor.Call
	implementations, reviews := 0, 0
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, req)
		if err != nil {
			return answer, err
		}
		if !req.Review {
			implementations++
			if implementations == 2 {
				if err := os.WriteFile(filepath.Join(req.Policy.Tree, "README.md"), []byte("revised\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			return answer, nil
		}
		reviews++
		if reviews == 1 {
			var v map[string]any
			json.Unmarshal(answer.JSON, &v)
			v["decision"] = "hold"
			v["findings"] = []string{"must fix"}
			v["findingKinds"] = []string{"security"}
			v["findingSeverities"] = []string{"must"}
			answer.JSON = encode(v)
		}
		return answer, nil
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || !reflect.DeepEqual(events, []string{"actual:feature.txt", "add:README.md"}) {
		t.Fatalf("state=%+v err=%v events=%v", state, err, events)
	}
}

func TestNarrowScopeClosedServerContinuesAndCounts(t *testing.T) {
	f := newPipeline(t, "approve")
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	server.Close()
	f.Executor.API = api
	d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Scope: []string{"*"}}}
	r := &journal.Record{ID: 1}
	err = f.Executor.narrowScope(context.Background(), &d, 3, []string{"feature.txt"}, func(context.Context) error { return nil }, func(context.Context) error { return nil }, r, map[string]bool{})
	saved, loadErr := f.Executor.Store.Load(1)
	if err != nil || loadErr != nil || saved == nil || saved.ScopeNarrowFailed != 1 || saved.ScopeNarrowed != 0 || !reflect.DeepEqual(d.Scope, []string{"*"}) {
		t.Fatalf("err=%v load=%v saved=%+v scope=%v", err, loadErr, saved, d.Scope)
	}
}

func TestNarrowScopePipelinePushAndPersist(t *testing.T) {
	f := newPipeline(t, "approve")
	calls := 0
	f.ScopeActual = func(w http.ResponseWriter, paths []string) {
		calls++
		contents := f.git(f.root, "--git-dir", f.bare, "show", "refs/heads/"+f.Branch+":feature.txt")
		if contents != "accepted feature" {
			t.Errorf("remote contents=%q", contents)
		}
		fmt.Fprint(w, `{"result":"granted","job":{"id":1,"scope":["feature.txt"]}}`)
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	saved, loadErr := f.Executor.Store.Load(1)
	if err != nil || state.Status != "merged" || calls != 1 || loadErr != nil || saved == nil || saved.ScopeNarrowed != 1 || saved.ScopeNarrowFailed != 0 {
		t.Fatalf("state=%+v err=%v calls=%d saved=%+v load=%v", state, err, calls, saved, loadErr)
	}
}

func TestNarrowScopeExecuteAlreadyNarrowed(t *testing.T) {
	f := newPipeline(t, "approve")
	var d Descriptor
	if err := json.Unmarshal(f.Job.Raw, &d); err != nil {
		t.Fatal(err)
	}
	d.Scope = []string{"feature.txt"}
	f.Job.Raw = encode(d)
	calls := 0
	f.ScopeActual = func(w http.ResponseWriter, paths []string) { calls++ }
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "merged" || calls != 0 {
		t.Fatalf("state=%+v err=%v calls=%d", state, err, calls)
	}
}

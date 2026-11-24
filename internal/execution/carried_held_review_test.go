package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestCarriedHeldReviewWireRoundTrip(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "review-hold")
	f.AllowUsage = true
	ctx := context.Background()
	// Nested objects and HTML-sensitive characters are the parts whose bytes a
	// jsonb store rewrites.
	finding := "a<b && c>d のとき検査を迂回できる"
	reviewer := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := reviewer(ctx, req)
		if req.Review && err == nil {
			answer.JSON = changeSecondReview(secondReviewResponse("security", finding), "breaking", true)
		}
		return answer, err
	}
	state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" {
		t.Fatalf("PC-A: %v %v", state.Status, err)
	}
	var worker struct {
		Held map[string]any `json:"carriedHeldReview"`
	}
	var view struct {
		Worker json.RawMessage `json:"workerExecution"`
	}
	if json.Unmarshal(state.View, &view) != nil || json.Unmarshal(view.Worker, &worker) != nil || worker.Held["version"] != float64(1) {
		t.Fatalf("missing held state: %s", state.View)
	}
	if strings.Contains(string(view.Worker), "diff --git") || worker.Held["review"] != nil {
		t.Fatal("patch or review in checkpoint")
	}
	var saved intake.State
	requests := 0
	failNext := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+"idk_"+strings.Repeat("a", 43) {
			t.Error("missing authentication")
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/jobs/1/state":
			var body struct {
				Epoch int64 `json:"epoch"`
				intake.State
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Epoch != 3 {
				t.Error("invalid epoch")
				w.WriteHeader(409)
				return
			}
			saved = body.State
			saved.View, saved.Result = storedJSON(t, saved.View), storedJSON(t, saved.Result)
			io.WriteString(w, `{"job":{"id":1}}`)
		case "/api/v1/jobs/next":
			requests++
			if r.Header.Get(intake.ContractHeader) != intake.ContractVersion {
				t.Error("missing contract")
				w.WriteHeader(409)
				return
			}
			if failNext {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			var job map[string]any
			_ = json.Unmarshal(f.Job.Raw, &job)
			job["view"], job["result"], job["branch"], job["headSha"], job["pr"] = json.RawMessage(saved.View), json.RawMessage(saved.Result), saved.Branch, saved.HeadSHA, saved.PR
			job["leaseEpoch"] = 4
			_ = json.NewEncoder(w).Encode(map[string]any{"job": job, "lease": map[string]any{"epoch": 4, "expiresAt": time.Now().Add(time.Hour)}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	if err = client.State(ctx, 1, 3, state); err != nil {
		t.Fatal(err)
	}
	assignment, err := client.NextFor(ctx, 1, nil)
	if err != nil || assignment.Job == nil || requests != 1 {
		t.Fatalf("next: %+v %v calls=%d", assignment, err, requests)
	}
	var descriptor Descriptor
	if err := json.Unmarshal(assignment.Job.Raw, &descriptor); err != nil {
		t.Fatal(err)
	}
	// PC-B uses a separate runtime and journal and enters the real Executor.
	root := t.TempDir()
	store, err := journal.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := f.Executor.Config
	cfg.RuntimeDir = root
	b := &Executor{Config: cfg, API: f.API, Gitea: f.Executor.Gitea, Store: store, Executable: f.Executor.Executable, ready: true}
	f.CurrentLeaseEpoch = 4
	f.CurrentJobHead = saved.HeadSHA
	_ = json.Unmarshal(saved.View, &f.CurrentView)
	var observed *journal.CarriedHeldReview
	var observedRecord *journal.Record
	b.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		record, loadErr := store.Load(1)
		if loadErr != nil {
			t.Error(loadErr)
		} else {
			observed = record.CarriedHeldReview
			observedRecord = record
		}
		return providers.Answer{}, errors.New("fixture stop after restoration")
	}
	_, _ = b.Execute(ctx, *assignment.Job, func(context.Context) error { return nil })
	// The matching state now ends in a hold without an AI call.
	if observedRecord == nil {
		if observedRecord, err = store.Load(1); err == nil {
			observed = observedRecord.CarriedHeldReview
		}
	}
	if observed == nil || observed.DiffHash != worker.Held["diffHash"] {
		t.Fatalf("PC-B did not restore before execution: %+v descriptor=%s", observed, assignment.Job.Raw)
	}
	var restored Review
	if json.Unmarshal(observed.Review, &restored) != nil || len(restored.FindingSpecs) != 1 || restored.FindingSpecs[0].Problem != finding {
		t.Fatalf("restored review lost its finding: %s", observed.Review)
	}
	if bytes.Equal(saved.Result, state.Result) {
		t.Fatal("the fixture did not rewrite the stored JSON")
	}
	patch, err := os.ReadFile(reviewBaselinePath(f.Executor.Config.RuntimeDir, 1))
	if err != nil {
		t.Fatal(err)
	}
	request := reviewRequestHash(descriptor, "Implement feature", "受入条件: feature.txtを生成する")
	if !carriedHeldMatches(observedRecord, descriptor, request, string(patch)) {
		t.Fatal("received evidence does not match the original diff")
	}
	if carriedHeldMatches(observedRecord, descriptor, request, string(patch)+"corrupt\n") || carriedHeldMatches(observedRecord, descriptor, digestReview("changed"), string(patch)) {
		t.Fatal("incomparable diff or request accepted")
	}

	originalView, originalResult := append(json.RawMessage(nil), saved.View...), append(json.RawMessage(nil), saved.Result...)
	for _, tc := range []struct {
		name  string
		alter func(map[string]any, map[string]any)
	}{
		{"missing", func(w, _ map[string]any) { delete(w, "carriedHeldReview") }},
		{"broken", func(w, _ map[string]any) { w["carriedHeldReview"] = "broken" }},
		{"version", func(w, _ map[string]any) { w["carriedHeldReview"].(map[string]any)["version"] = 2 }},
		{"target", func(w, _ map[string]any) { w["carriedHeldReview"].(map[string]any)["issue"] = 9 }},
		{"generation", func(w, _ map[string]any) { w["carriedHeldReview"].(map[string]any)["generation"] = 1 }},
		{"missing generation", func(w, _ map[string]any) { delete(w["carriedHeldReview"].(map[string]any), "generation") }},
		{"unknown order", func(w, _ map[string]any) { w["carriedHeldReview"].(map[string]any)["epoch"] = 4 }},
		{"review mismatch", func(_ map[string]any, result map[string]any) {
			result["review"] = map[string]any{"decision": "approve"}
		}},
		// Last: the fixture view keeps retryFromBase once it has been delivered.
		{"explicit retry", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v, result map[string]any
			_ = json.Unmarshal(originalView, &v)
			_ = json.Unmarshal(originalResult, &result)
			worker := v["workerExecution"].(map[string]any)
			if tc.alter == nil {
				v["retryFromBase"] = map[string]any{"generation": 0}
			} else {
				tc.alter(worker, result)
			}
			saved.View, saved.Result = encode(v), encode(result)
			_ = json.Unmarshal(saved.View, &f.CurrentView)
			a, nextErr := client.NextFor(ctx, 1, nil)
			if nextErr != nil || a.Job == nil {
				t.Fatalf("next: %v", nextErr)
			}
			root := t.TempDir()
			st, openErr := journal.Open(root)
			if openErr != nil {
				t.Fatal(openErr)
			}
			defer st.Close()
			cfg := f.Executor.Config
			cfg.RuntimeDir = root
			ex := &Executor{Config: cfg, API: f.API, Gitea: f.Executor.Gitea, Store: st, Executable: f.Executor.Executable, ready: true}
			called := false
			ex.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				called = true
				record, loadErr := st.Load(1)
				if loadErr != nil || record.CarriedHeldReview != nil {
					t.Errorf("invalid checkpoint restored: %+v %v", record, loadErr)
				}
				return providers.Answer{}, errors.New("fixture stop after restoration")
			}
			_, _ = ex.Execute(ctx, *a.Job, func(context.Context) error { return nil })
			if !called {
				t.Fatal("did not reach the product execution path")
			}
		})
	}
	before := requests
	failNext = true
	if _, err := client.NextFor(ctx, 1, nil); err == nil || requests != before+1 {
		t.Fatalf("failed acquisition retried: err=%v requests=%d", err, requests-before)
	}
}

// storedJSON rewrites a report the way the intake does before delivering it
// again: decoded into generic values and re-encoded without HTML escaping.
func storedJSON(t *testing.T, raw json.RawMessage) json.RawMessage {
	t.Helper()
	if len(raw) == 0 {
		return raw
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(out.Bytes())
}

func TestCarriedHeldReviewWorkerOwnership(t *testing.T) {
	t.Parallel()
	f := newPipeline(t, "review-hold")
	f.AllowUsage = true
	original := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, req)
		if !req.Review && err == nil {
			var obj map[string]any
			if json.Unmarshal(answer.JSON, &obj) != nil {
				t.Fatal("invalid fixture implementation")
			}
			obj["carriedHeldReview"] = map[string]any{"decision": "held", "epoch": 999}
			answer.JSON = encode(obj)
		}
		if req.Review && err == nil {
			var obj map[string]any
			if json.Unmarshal(answer.JSON, &obj) != nil {
				t.Fatal("invalid fixture review")
			}
			obj["carriedHeldReview"] = nil
			obj["reviewedEpoch"] = 999
			obj["reviewedHead"] = strings.Repeat("f", 40)
			answer.JSON = encode(obj)
		}
		return answer, err
	}
	state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	if err != nil || state.Status != "held" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
	var view struct {
		Worker struct {
			Held journal.CarriedHeldReview `json:"carriedHeldReview"`
		} `json:"workerExecution"`
	}
	if err := json.Unmarshal(state.View, &view); err != nil {
		t.Fatal(err)
	}
	if view.Worker.Held.Epoch != 3 || view.Worker.Held.Head != state.HeadSHA || view.Worker.Held.Decision != "held" {
		t.Fatalf("AI changed held evidence: %+v", view.Worker.Held)
	}
	var result struct {
		Review json.RawMessage `json:"review"`
	}
	if json.Unmarshal(state.Result, &result) != nil || !carriedHeldValid(&view.Worker.Held, result.Review) {
		t.Fatalf("Worker evidence does not match saved review: %s", state.Result)
	}
	// A later Worker review must win when the old state is delivered again.
	var desc Descriptor
	if err := json.Unmarshal(f.Job.Raw, &desc); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(state.View, &desc.View)
	// A new instruction changes the request, so PC-B reviews again instead of
	// applying the carried hold before implementation.
	desc.View["instructions"] = encode([]map[string]string{{"id": "new", "text": "変更を加える"}})
	desc.Result, desc.Branch, desc.HeadSHA = state.Result, state.Branch, state.HeadSHA
	if state.PR != nil {
		desc.PR = *state.PR
	}
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := f.Executor.Config
	cfg.RuntimeDir = t.TempDir()
	b := &Executor{Config: cfg, API: f.API, Gitea: f.Executor.Gitea, Store: store, Executable: f.Executor.Executable, ready: true, Call: original}
	f.CurrentLeaseEpoch, f.CurrentJobHead, f.CurrentView = 4, state.HeadSHA, desc.View
	job := f.Job
	job.LeaseEpoch = 4
	job.Raw = encode(desc)
	newer, err := b.Execute(context.Background(), job, func(context.Context) error { return nil })
	if err != nil || newer.Status != "held" {
		t.Fatalf("newer review: %s %v", newer.Status, err)
	}
	var nextView struct {
		Worker struct {
			Held journal.CarriedHeldReview `json:"carriedHeldReview"`
		} `json:"workerExecution"`
	}
	if json.Unmarshal(newer.View, &nextView) != nil || nextView.Worker.Held.Epoch != 4 {
		t.Fatalf("new review not retained: %s", newer.View)
	}
	record, err := store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	record.FinalReported = true
	if err := store.Save(record); err != nil {
		t.Fatal(err)
	}
	f.CurrentLeaseEpoch = 5
	job.LeaseEpoch = 5
	_, _ = b.Execute(context.Background(), job, func(context.Context) error { return nil })
	record, err = store.Load(1)
	if err != nil || record.ReviewBaseline == nil || carriedReviewDigest(record.ReviewBaseline.Review) != nextView.Worker.Held.ReviewHash {
		t.Fatalf("old state replaced newer review: %+v %v", record, err)
	}

	// Generate deferred items and a delivery through the Worker, then produce
	// another held review and carry it to a fresh journal.
	old, err := f.Executor.Store.Load(1)
	if err != nil {
		t.Fatal(err)
	}
	old.Review = changeSecondReview(old.Review, "deferredReviewItems", []followupProposalItem{{Problem: "retained proposal", Expected: fixtureEvidence + "期待: retained proposal", Test: fixtureVerification + " 検査: retained proposal"}})
	review, raw, _ := decideReview(old, encode(map[string]any{"decision": "approve"}))
	if !review.Approved() {
		t.Fatal("expected an actionable deferred review")
	}
	old.Review = raw
	f.CurrentLeaseEpoch = 3
	if err := f.Executor.postFollowupProposals(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if len(old.FollowupProposals) == 0 {
		t.Fatal("delivery was not recorded")
	}
	// Repeated long text pushes the final report over the intake limit, so the
	// review travels as an archive, and the store rewrites that archive too.
	repeated := make([]string, 60)
	for i := range repeated {
		repeated[i] = strings.Repeat("a<b && c>d ", 400)
	}
	review, raw, _ = decideReview(old, changeSecondReview(changeSecondReview(secondReviewResponse("security", "blocking finding"), "breaking", true), "postMergeChecks", repeated))
	old.Review = raw
	patch, err := os.ReadFile(reviewBaselinePath(f.Executor.Config.RuntimeDir, 1))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.saveReviewBaseline(old, gitwork.Snapshot{Head: old.Head, Base: old.Base, Diff: string(patch)}, review, raw, "full", reviewRequestHash(Descriptor{}, "Implement feature", "受入条件: feature.txtを生成する")); err != nil {
		t.Fatal(err)
	}
	retained, err := f.Executor.finish(old, "held", "review_held", "保留理由", nil)
	if err != nil {
		t.Fatal(err)
	}
	var retainedDesc Descriptor
	_ = json.Unmarshal(f.Job.Raw, &retainedDesc)
	if !strings.Contains(string(retained.Result), `"reviewArchive"`) {
		t.Fatal("the held review was not archived")
	}
	retained.View, retained.Result = storedJSON(t, retained.View), storedJSON(t, retained.Result)
	_ = json.Unmarshal(retained.View, &retainedDesc.View)
	retainedDesc.Result, retainedDesc.HeadSHA, retainedDesc.Branch = retained.Result, retained.HeadSHA, retained.Branch
	if retained.PR != nil {
		retainedDesc.PR = *retained.PR
	}
	retainedStore, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer retainedStore.Close()
	retainedCfg := f.Executor.Config
	retainedCfg.RuntimeDir = t.TempDir()
	retainedWorker := &Executor{Config: retainedCfg, API: f.API, Gitea: f.Executor.Gitea, Store: retainedStore, Executable: f.Executor.Executable, ready: true}
	f.CurrentLeaseEpoch, f.CurrentView = 4, retainedDesc.View
	checked := false
	check := func() {
		got, loadErr := retainedStore.Load(1)
		if loadErr != nil {
			t.Error(loadErr)
			return
		}
		var carried Review
		_ = json.Unmarshal(firstJSON(got.Review, got.PreviousReview), &carried)
		checked = got.CarriedHeldReview != nil && len(carried.DeferredReviewItems) > 0 && len(carried.DeferredReviewDeliveries) > 0 && len(got.FollowupProposals) > 0
		if !checked {
			t.Logf("held=%+v items=%d deliveries=%d proposals=%d previous=%s", got.CarriedHeldReview, len(carried.DeferredReviewItems), len(carried.DeferredReviewDeliveries), len(got.FollowupProposals), got.PreviousReview)
		}
	}
	called := false
	retainedWorker.Call = func(context.Context, providers.Request) (providers.Answer, error) {
		called = true
		check()
		return providers.Answer{}, errors.New("stop")
	}
	job.LeaseEpoch, job.Raw = 4, encode(retainedDesc)
	_, _ = retainedWorker.Execute(context.Background(), job, func(context.Context) error { return nil })
	if !called {
		// A matching carried hold ends without an AI call.
		check()
	}
	if !checked {
		t.Fatal("retained items and delivery were not restored with held evidence")
	}
	// A new instruction invalidates the local evidence; redelivery of the old
	// checkpoint in a later lease must not recreate it.
	for _, instruction := range []bool{true, false} {
		current, loadErr := retainedStore.Load(1)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		current.FinalReported = true
		if err := retainedStore.Save(current); err != nil {
			t.Fatal(err)
		}
		if instruction {
			retainedDesc.View["instructions"] = encode([]map[string]string{{"id": "new", "text": "変更を加える"}})
		} else {
			delete(retainedDesc.View, "instructions")
		}
		f.CurrentLeaseEpoch++
		f.CurrentView = retainedDesc.View
		job.LeaseEpoch, job.Raw = f.CurrentLeaseEpoch, encode(retainedDesc)
		seen := false
		retainedWorker.Call = func(context.Context, providers.Request) (providers.Answer, error) {
			current, loadErr := retainedStore.Load(1)
			if loadErr != nil {
				t.Error(loadErr)
			} else if current.CarriedHeldReview != nil {
				t.Error("invalidated state was restored")
			}
			seen = true
			return providers.Answer{}, errors.New("stop")
		}
		_, _ = retainedWorker.Execute(context.Background(), job, func(context.Context) error { return nil })
		if !seen {
			t.Fatal("invalidation did not reach execution")
		}
	}
}

// A journal which already holds carried evidence must drop it when the work it
// described is superseded; the delivered checkpoint stays unchanged throughout.
func TestCarriedHeldReviewInvalidation(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"generation", "retry", "instruction", "newer review", "incomparable review"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			f := newPipeline(t, "review-hold")
			f.AllowUsage = true
			ctx := context.Background()
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "held" {
				t.Fatalf("PC-A: %v %v", state.Status, err)
			}
			var desc Descriptor
			if err := json.Unmarshal(f.Job.Raw, &desc); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(state.View, &desc.View)
			desc.Result, desc.Branch, desc.HeadSHA = state.Result, state.Branch, state.HeadSHA
			if state.PR != nil {
				desc.PR = *state.PR
			}
			store, err := journal.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cfg := f.Executor.Config
			cfg.RuntimeDir = t.TempDir()
			b := &Executor{Config: cfg, API: f.API, Gitea: f.Executor.Gitea, Store: store, Executable: f.Executor.Executable, ready: true}
			var seen *journal.Record
			b.Call = func(context.Context, providers.Request) (providers.Answer, error) {
				record, loadErr := store.Load(1)
				if loadErr != nil {
					t.Error(loadErr)
				}
				seen = record
				return providers.Answer{}, errors.New("stop")
			}
			job := f.Job
			run := func(epoch int64, fence func(context.Context) error) intake.State {
				f.CurrentLeaseEpoch, f.CurrentJobHead, f.CurrentView = epoch, state.HeadSHA, desc.View
				job.LeaseEpoch, job.Raw = epoch, encode(desc)
				seen = nil
				out, _ := b.Execute(ctx, job, fence)
				if seen == nil && epoch == 4 {
					// A matching state holds without an AI call.
					seen, _ = store.Load(1)
				}
				return out
			}
			run(4, func(context.Context) error { return nil })
			if seen == nil || seen.CarriedHeldReview == nil {
				t.Fatalf("PC-B did not restore the held state: %+v", seen)
			}
			current, err := store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			current.FinalReported = true
			if err := store.Save(current); err != nil {
				t.Fatal(err)
			}
			var result map[string]json.RawMessage
			if err := json.Unmarshal(state.Result, &result); err != nil {
				t.Fatal(err)
			}
			fence := func(context.Context) error { return nil }
			switch path {
			case "generation":
				desc.Generation++
			case "retry":
				desc.View["retryFromBase"] = json.RawMessage(`{"generation":0}`)
			case "instruction":
				fence = func(context.Context) error {
					control := b.controls.get(job.ID)
					control.mu.Lock()
					control.reason = "instruction"
					control.mu.Unlock()
					return errors.New("interrupt for instruction")
				}
			case "newer review":
				result["review"] = changeSecondReview(changeSecondReview(result["review"], "summary", "別PCの新しいレビュー"), "reviewedEpoch", 5)
				desc.Result = encode(result)
			case "incomparable review":
				result["review"] = json.RawMessage(`{"decision":"hold"}`)
				desc.Result = encode(result)
			}
			out := run(6, fence)
			if path == "instruction" {
				if out.Status != "queued" || strings.Contains(string(out.View), "carriedHeldReview") {
					t.Fatalf("interruption reported held evidence: %s %s", out.Status, out.View)
				}
				if seen, err = store.Load(1); err != nil {
					t.Fatal(err)
				}
			}
			if seen == nil {
				t.Fatal("did not reach the product execution path")
			}
			if seen.CarriedHeldReview != nil {
				t.Fatalf("superseded held state kept: %+v", seen.CarriedHeldReview)
			}
		})
	}
}

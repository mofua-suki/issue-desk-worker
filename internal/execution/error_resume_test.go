package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/jobspec"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	workerruntime "issue-desk-worker/internal/runtime"
)

// The execution pipeline, journal and result endpoint are the repository's
// existing integration fixture. Only acquisition, heartbeat and lease are fake.
type errorResumeAPI struct {
	*intake.Client
	assignment   intake.Assignment
	next, states int
	errorReports int
	errorKind    string
	errorDetail  map[string]string
	rejectFinal  bool
}

func (a *errorResumeAPI) Heartbeat(context.Context, intake.HostStatus) error { return nil }
func (a *errorResumeAPI) Next(context.Context, int) (intake.Assignment, error) {
	a.next++
	if a.next == 1 {
		return a.assignment, nil
	}
	return intake.Assignment{}, &intake.APIError{Status: http.StatusForbidden}
}
func (a *errorResumeAPI) Renew(context.Context, int64, int64) (intake.Assignment, error) {
	return a.assignment, nil
}
func (a *errorResumeAPI) State(ctx context.Context, id, epoch int64, state intake.State) error {
	a.states++
	if a.rejectFinal && state.Status == "held" {
		return &intake.APIError{Status: 400, Code: "request_failed"}
	}
	var view map[string]json.RawMessage
	if state.Status == "running" && len(state.Result) == 0 && json.Unmarshal(state.View, &view) == nil && len(view) == 1 && len(view["lastExecutionError"]) > 0 {
		a.errorReports++
		var detail struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(view["lastExecutionError"], &a.errorDetail)
		// Keep errorKind empty when the reported detail cannot be decoded.
		a.errorKind = ""
		if err := json.Unmarshal(view["lastExecutionError"], &detail); err == nil {
			a.errorKind = detail.Kind
		}
	}
	return a.Client.State(ctx, id, epoch, state)
}
func runErrorResume(t *testing.T, f *pipelineFixture) *errorResumeAPI {
	t.Helper()
	a := &errorResumeAPI{Client: f.API, assignment: intake.Assignment{Job: &f.Job, Lease: &intake.Lease{Epoch: f.Job.LeaseEpoch, ExpiresAt: time.Now().Add(time.Hour)}}}
	return runErrorResumeWithAPI(t, f, a)
}
func runErrorResumeWithAPI(t *testing.T, f *pipelineFixture, a *errorResumeAPI) *errorResumeAPI {
	t.Helper()
	r := workerruntime.Runner{API: a, Executor: f.Executor, Capacity: 2}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	err := r.Run(ctx)
	var refusal *intake.APIError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusForbidden {
		t.Fatalf("execution did not drain after acquisition refusal: %v", err)
	}
	if a.next != 2 {
		t.Fatal("acquisition was retried", a.next)
	}
	return a
}

func TestRuntimeErrorLeavesUncertainMergeForRedistribution(t *testing.T) {
	f := newPipeline(t, "approve")
	// Both the applied merge's response and its subsequent verification fail.
	f.MergeFailStatus, f.PullFailAfterPost, f.MergeFailApplied = 503, 503, true
	a := runErrorResume(t, f)
	saved, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil || saved == nil {
		t.Fatal(saved, err)
	}
	if saved.Stage != "merge" || hasJSON(saved.FinalState) || saved.FinalReported || a.states != 1 || a.errorReports != 1 || !f.merged {
		t.Fatal("uncertain merge was finalized or published", saved, a.states)
	}
	posts := f.MergePosts
	f.MergeFailStatus, f.PullFailAfterPost = 0, 0
	// Model explicit redistribution; this test does not retry a real job.
	a = runErrorResume(t, f)
	saved, err = f.Executor.Store.Load(f.Job.ID)
	if err != nil || saved.Stage != "done" || !saved.FinalReported || a.states != 1 || f.MergePosts != posts {
		t.Fatal("redistributed merge was not reconciled and acknowledged", saved, err, a.states)
	}
}

func TestRuntimeErrorDoesNotResendOrReplaceSavedResults(t *testing.T) {
	for _, status := range []string{"held", "failed", "stopped", "merged"} {
		for _, reported := range []bool{false, true} {
			t.Run(status+map[bool]string{false: "/pending", true: "/reported"}[reported], func(t *testing.T) {
				f := newPipeline(t, "approve")
				r := &journal.Record{ID: f.Job.ID, Repo: f.Job.Repo, Issue: 7, Epoch: f.Job.LeaseEpoch}
				if _, err := f.Executor.finish(r, status, "", "confirmed result", nil); err != nil {
					t.Fatal(err)
				}
				if reported {
					if err := f.Executor.Reported(r.ID, r.Epoch); err != nil {
						t.Fatal(err)
					}
				}
				before, err := f.Executor.Store.Load(r.ID)
				if err != nil {
					t.Fatal(err)
				}
				outage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
				defer outage.Close()
				f.Executor.Gitea, err = gitea.New(outage.URL, "fixture")
				if err != nil {
					t.Fatal(err)
				}
				a := runErrorResume(t, f)
				after, err := f.Executor.Store.Load(r.ID)
				if a.errorKind != "gitea_api_error" {
					t.Fatalf("unexpected reported classification: %q", a.errorKind)
				}
				if err != nil || !bytes.Equal(encode(before), encode(after)) || a.states != 1 || a.errorReports != 1 {
					t.Fatal("error resent or changed a saved result", before, after, err, a.states)
				}
			})
		}
	}
}

func TestRuntimeErrorRetainsUsageUntilAcknowledged(t *testing.T) {
	f := newPipeline(t, "approve")
	s := &limitServer{rejected: 1}
	f.Executor.API = s.client(t)
	r := &journal.Record{ID: f.Job.ID, Repo: f.Job.Repo, Issue: 7, Epoch: f.Job.LeaseEpoch, Stage: "implement"}
	one := int64(1)
	if err := f.Executor.recordUsage(r, Descriptor{Descriptor: jobspec.Descriptor{Repo: r.Repo, Issue: r.Issue}}, providers.Settings{Provider: "codex", Model: "model"}, "implement", providers.Answer{Outcome: "completed", Usage: providers.Usage{Input: &one, Output: &one}}); err != nil {
		t.Fatal(err)
	}
	a := runErrorResume(t, f)
	saved, err := f.Executor.Store.Load(r.ID)
	if err != nil || saved == nil {
		t.Fatal(saved, err)
	}
	if saved.UsageSent != 0 || len(saved.Usage) != 1 || hasJSON(saved.FinalState) || saved.FinalReported || a.states != 1 || a.errorReports != 1 {
		t.Fatal("usage refusal synthesized/acknowledged a result", saved, a.states)
	}
	original := append([]byte(nil), saved.Usage[0]...)
	s.rejected = 0
	if err := f.Executor.flushUsage(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	saved, err = f.Executor.Store.Load(r.ID)
	if err != nil || saved.UsageSent != 1 || !bytes.Equal(original, saved.Usage[0]) || hasJSON(saved.FinalState) || saved.FinalReported {
		t.Fatal("usage was not acknowledged independently with its original content", saved, err)
	}
}

func TestRuntimeMergePreflightHoldAcknowledged(t *testing.T) {
	f := newPipeline(t, "approve")
	f.Reviews = []map[string]any{{"state": "REQUEST_CHANGES"}}
	a := runErrorResume(t, f)
	saved, err := f.Executor.Store.Load(f.Job.ID)
	if err != nil || saved == nil || saved.Stage != "held" || !saved.FinalReported || a.errorReports != 0 || f.MergePosts != 0 {
		t.Fatalf("hold not acknowledged: saved=%+v err=%v errors=%d", saved, err, a.errorReports)
	}
	if len(f.ReceivedStates) == 0 || f.ReceivedStates[len(f.ReceivedStates)-1].Status != "held" {
		t.Fatal("intake did not receive held")
	}
}

func TestPublicationErrorsAreSafeDiagnostics(t *testing.T) {
	for _, reason := range publicationErrors {
		diagnostic := &ExecutionError{Err: reason, Stage: "merge"}
		if got := diagnostic.SafeExecutionReason(); got != reason.Error() {
			t.Errorf("publication diagnostic: got %q, want %q", got, reason.Error())
		}
	}
}

func TestExecutionDiagnosticRejectsUnlistedErrors(t *testing.T) {
	for _, message := range []string{"response body secret", "https://example.invalid/?token=secret", "Bearer secret", "変更要求のレビューが残っています: secret", ""} {
		err := &ExecutionError{Err: errors.New(message), Stage: "publish"}
		if got := err.SafeExecutionReason(); got != "" {
			t.Fatalf("unsafe reason %q", got)
		}
	}
	cause := errors.New("変更要求のレビューが残っています")
	err := &ExecutionError{Err: cause, Stage: "publish"}
	var got *ExecutionError
	if !errors.Is(err, cause) || !errors.As(err, &got) || got.SafeExecutionReason() != cause.Error() {
		t.Fatal("error chain or allowlist lost")
	}
	if failureStage(nil) != "" || failureStage(&journal.Record{Stage: "secret"}) != "" {
		t.Fatal("unknown stage disclosed")
	}
	for _, stage := range []string{"implement", "implement_report", "test", "test_report", "review", "publish", "merge", "deploy"} {
		if got := failureStage(&journal.Record{Stage: stage}); got != stage {
			t.Errorf("stage %q classified as %q", stage, got)
		}
	}
}

func TestExecuteDoesNotReportStaleStageOnEarlyFailure(t *testing.T) {
	f := newPipeline(t, "approve")
	if err := f.Executor.Store.Save(&journal.Record{ID: f.Job.ID, Repo: f.Job.Repo, Issue: 7, Epoch: f.Job.LeaseEpoch - 1, Stage: "publish"}); err != nil {
		t.Fatal(err)
	}
	f.Job.Raw = json.RawMessage(`{}`)
	_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	var diagnostic *ExecutionError
	if !errors.As(err, &diagnostic) || diagnostic.Stage != "" {
		t.Fatalf("stale stage: %v", err)
	}
}

func TestCheckpointNotificationFailureReportsDestinationStage(t *testing.T) {
	f := newPipeline(t, "approve")
	call := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, request providers.Request) (providers.Answer, error) {
		answer, err := call(ctx, request)
		if !request.Review {
			f.StateStatus = http.StatusBadRequest
		}
		return answer, err
	}
	_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
	var diagnostic *ExecutionError
	if !errors.As(err, &diagnostic) {
		t.Fatalf("expected execution error: %v", err)
	}
	saved, loadErr := f.Executor.Store.Load(f.Job.ID)
	if loadErr != nil || saved == nil {
		t.Fatalf("load journal: %v", loadErr)
	}
	if diagnostic.Stage != "test" || saved.Stage != diagnostic.Stage {
		t.Fatalf("reported=%q saved=%q", diagnostic.Stage, saved.Stage)
	}
}

type failingStageStore struct{ err error }

func (s failingStageStore) Save(*journal.Record) error { return s.err }

func TestStageSaveFailureReportsDestination(t *testing.T) {
	for _, destination := range []string{"test", "merge"} {
		t.Run(destination, func(t *testing.T) {
			f := newPipeline(t, "approve")
			failure := errors.New("save failed")
			store := &selectiveStageStore{destination: destination, store: f.Executor.Store, err: failure}
			if destination == "merge" {
				store.failAt = 2
			} else {
				store.failAt = 1
			}
			f.Executor.stageStore = store
			_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			var diagnostic *ExecutionError
			if !errors.As(err, &diagnostic) || !errors.Is(err, failure) || diagnostic.Stage != destination {
				t.Fatalf("stage=%v error=%v", diagnostic, err)
			}
			saved, err := f.Executor.Store.Load(f.Job.ID)
			if err != nil || saved == nil || (destination == "test" && saved.Stage == destination) || (destination == "merge" && saved.Stage != destination) || store.matches != store.failAt {
				t.Fatalf("saved=%v error=%v", saved, err)
			}
		})
	}
}

type selectiveStageStore struct {
	destination string
	store       *journal.Store
	err         error
	matches     int
	failAt      int
}

func (s *selectiveStageStore) Save(r *journal.Record) error {
	if r.Stage == s.destination {
		s.matches++
		if s.matches == s.failAt {
			return failingStageStore{s.err}.Save(r)
		}
	}
	return s.store.Save(r)
}

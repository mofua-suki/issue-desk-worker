package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"issue-desk-worker/internal/jobspec"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/gitwork"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestSecondReviewInvalidCarryCannotUseOlderComparison(t *testing.T) {
	for _, damage := range []string{"generation", "epoch", "head", "malformed", "omitted", "classification", "local", "archive"} {
		t.Run(damage, func(t *testing.T) {
			old := stampReviewEvidence(secondReviewResponse("data", "old"), 1, 1, strings.Repeat("a", 40))
			incoming := stampReviewEvidence(secondReviewResponse("data", "latest"), 2, 2, strings.Repeat("b", 40))
			switch damage {
			case "generation":
				incoming = changeSecondReview(incoming, "reviewedGeneration", -1)
			case "epoch":
				incoming = changeSecondReview(incoming, "reviewedEpoch", -1)
			case "head":
				incoming = changeSecondReview(incoming, "reviewedHead", "invalid")
			case "malformed":
				incoming = json.RawMessage(`"broken report"`)
			case "omitted":
				incoming = json.RawMessage(`{"omitted":true}`)
			case "classification":
				incoming = changeSecondReview(incoming, "findingKinds", []string{})
			case "archive":
				incoming = encode(map[string]any{"reviewArchive": reviewArchive{Version: 1, Value: encode(map[string]string{"decision": "999"})}})
			case "local":
				old = changeSecondReview(old, "reviewedGeneration", -1)
			}
			r := &journal.Record{Generation: 2, Epoch: 3, Review: old, Head: strings.Repeat("c", 40), ReviewHolds: 3}
			seedPreviousResults(r, Descriptor{Descriptor: jobspec.Descriptor{Generation: 3, Result: encode(map[string]any{"review": incoming, "previousReview": secondReviewResponse("data", "fallback")})}})
			review, raw, _ := decideReview(r, secondReviewResponse("acceptance", "latest"))
			if review.Approved() || !reflect.DeepEqual(review.Severities, []string{"must"}) || len(review.DeferredReviewItems) != 0 {
				t.Fatalf("invalid evidence bypassed: %s", raw)
			}
		})
	}
}

func TestSecondReviewApprovedCarryDelivery(t *testing.T) {
	for _, stage := range []string{"publish", "review"} {
		t.Run(stage, func(t *testing.T) {
			f := newPipeline(t, "approve")
			ctx := context.Background()
			c := gitwork.Config{Git: f.Executor.Config.Git, Root: f.Executor.Config.RuntimeDir, BaseURL: f.Executor.Config.GiteaURL, TokenFile: f.Executor.Config.WorkerTokenFile, Executable: f.Executor.Executable, User: f.Executor.Config.GitUser}
			w, err := gitwork.Open(ctx, c, "owner/project", "main", f.Branch, 1)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(w.Tree, "feature.txt"), []byte("accepted feature\n"), 0600); err != nil {
				t.Fatal(err)
			}
			f.git(w.Tree, "add", "feature.txt")
			f.git(w.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "feature")
			snapshot, err := w.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			localItem := followupProposalItem{Problem: "local detail", Expected: fixtureEvidence + "local expected", Test: fixtureVerification + " local test"}
			incomingItem := followupProposalItem{Problem: "incoming detail", Expected: fixtureEvidence + "incoming expected", Test: fixtureVerification + " incoming test"}
			base := changeSecondReview(secondReviewResponse("other", localItem.Problem), "findingSeverities", []string{"minor"})
			base = changeSecondReview(base, "findingSpecs", []reviewFindingSpec{{Problem: localItem.Problem, Expected: localItem.Expected, Test: localItem.Test}})
			review, raw := normalizeReview(base)
			raw = stampReviewEvidence(raw, 0, 3, snapshot.Head)
			r := &journal.Record{ID: 1, Repo: "owner/project", Issue: 7, Stage: stage, Branch: f.Branch, Provider: "codex", Head: snapshot.Head, Base: snapshot.Base, Epoch: 3, Review: raw, Result: implementationFixture(), Test: encode(map[string]bool{"passed": true})}
			fixtureRecordEvidence(t, f, r, w.Tree)
			if err := os.MkdirAll(filepath.Dir(reviewBaselinePath(c.Root, 1)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := f.Executor.saveReviewBaseline(r, snapshot, review, raw, "full", reviewRequestHash(Descriptor{}, "Implement feature", "受入条件: feature.txtを生成する")); err != nil {
				t.Fatal(err)
			}
			incoming := stampReviewEvidence(secondReviewResponse("data", "older problem"), 0, 2, snapshot.Head)
			incoming = changeSecondReview(incoming, "deferredReviewItems", []followupProposalItem{incomingItem})
			var d Descriptor
			_ = json.Unmarshal(f.Job.Raw, &d)
			d.Result = encode(map[string]any{"review": incoming})
			f.Job.Raw = encode(d)
			state, err := f.Executor.Execute(ctx, f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "merged" || len(f.Calls) != 0 {
				t.Fatalf("approval should survive carry/reuse: %+v err=%v calls=%v", state, err, f.Calls)
			}
			bodies := proposalBodies(t, f)
			var items []followupProposalItem
			if len(bodies) != 1 || json.Unmarshal(bodies[0]["items"], &items) != nil || !reflect.DeepEqual(items, []followupProposalItem{localItem, incomingItem}) {
				t.Fatalf("missing approved carry details: %v", bodies)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil || len(saved.FollowupProposals) != 1 || saved.FollowupProposals[0].Status != "sent" {
				t.Fatal(saved, err)
			}
			if _, valid := loadReviewBaseline(c.Root, saved); !valid || stage == "review" && !saved.ReviewSkipped {
				t.Fatal("merged retention broke baseline reuse", saved.ReviewSkipped)
			}
		})
	}
}

func TestSecondReviewDeliveryHistoryAcrossGeneration(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "sent", true: "unknown"}[unknown], func(t *testing.T) {
			f := newPipeline(t, "permission")
			f.AllowUsage = true
			if unknown {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.Method == "GET" && strings.HasSuffix(req.URL.Path, "/issues/7/comments") {
						_, _ = io.WriteString(w, `[]`)
						return
					}
					if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/comments") {
						raw, err := io.ReadAll(req.Body)
						if err != nil {
							t.Error(err)
							return
						}
						req.Body = io.NopCloser(bytes.NewReader(raw))
						if bytes.Contains(raw, []byte("issue-desk-proposal ")) {
							f.DropNoticeResponse = true
							defer func() { f.DropNoticeResponse = false }()
						}
					}
					f.server.Config.Handler.ServeHTTP(w, req)
				}))
				t.Cleanup(server.Close)
				client, err := gitea.New(server.URL, "fixture-token")
				if err != nil {
					t.Fatal(err)
				}
				f.Executor.Gitea = client
			}
			original := f.Executor.Call
			reviews, implementations := 0, 0
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				a, err := original(ctx, req)
				if !req.Review {
					implementations++
					if err := os.WriteFile(filepath.Join(req.Policy.Tree, "revision.txt"), []byte(strings.Repeat("repair\n", implementations)), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					reviews++
					if reviews == 1 {
						a.JSON = secondReviewResponse("acceptance", "first problem")
					} else if reviews == 2 {
						a.JSON = secondReviewResponse("other", "retained proposal")
						external := true
						a.JSON = changeSecondReview(a.JSON, "findings", []string{"retained proposal", "稼働状態を調べる"})
						a.JSON = changeSecondReview(a.JSON, "findingKinds", []string{"other", "other"})
						a.JSON = changeSecondReview(a.JSON, "findingSeverities", []string{"minor", "minor"})
						a.JSON = changeSecondReview(a.JSON, "findingSpecs", []reviewFindingSpec{{Group: "other", Problem: "retained proposal", Expected: fixtureEvidence + "期待: retained proposal", Test: fixtureVerification + " 検査: retained proposal"}, {Group: "other", Problem: "稼働状態を調べる", Expected: "配備先が稼働", Test: "運用側で確認", ExternalCheckOnly: &external}})
					} else {
						a.JSON = changeSecondReview(a.JSON, "improvements", []string{"retained proposal", "new ordinary improvement"})
						item := fixtureProposal("new ordinary improvement")
						a.JSON = changeSecondReview(a.JSON, "findingSpecs", []reviewFindingSpec{{Problem: item.Problem, Expected: item.Expected, Test: item.Test}})
					}
				}
				return a, err
			}
			state, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "held" || reviews != 2 || len(proposalBodies(t, f)) != 1 {
				t.Fatalf("first publish/hold: %+v err=%v reviews=%d", state, err, reviews)
			}
			oldRecord, err := f.Executor.Store.Load(1)
			if err != nil || len(oldRecord.FollowupProposals) != 1 {
				t.Fatal(oldRecord, err)
			}
			firstDelivery := oldRecord.FollowupProposals[0]
			wantOutcome := "confirmed"
			if unknown {
				wantOutcome = "unknown"
			}
			if firstDelivery.DeliveryOutcome != wantOutcome {
				t.Fatalf("delivery outcome missing after publication: %+v", firstDelivery)
			}
			// Restore the saved retained-item form and its original delivery identity.
			var result map[string]json.RawMessage
			if err := json.Unmarshal(state.Result, &result); err != nil {
				t.Fatal(err)
			}
			var savedReview Review
			if err := json.Unmarshal(result["review"], &savedReview); err != nil {
				t.Fatal(err)
			}
			external := true
			savedReview.DeferredReviewItems = []followupProposalItem{{Problem: "retained proposal", Expected: fixtureEvidence + "期待: retained proposal", Test: fixtureVerification + " 検査: retained proposal"}, {Problem: "稼働状態を調べる", Expected: "配備先が稼働", Test: "運用側で確認", ExternalCheckOnly: &external}}
			for i := range savedReview.DeferredReviewDeliveries {
				savedReview.DeferredReviewDeliveries[i].Source = digestReview(reviewTextKey("retained proposal"))
			}
			var savedValue map[string]json.RawMessage
			_ = json.Unmarshal(result["review"], &savedValue)
			savedValue["deferredReviewItems"] = encode(savedReview.DeferredReviewItems)
			savedValue["deferredReviewDeliveries"] = encode(savedReview.DeferredReviewDeliveries)
			result["review"] = encode(savedValue)
			state.Result = encode(result)
			// Carry only the production final result to a PC without this journal
			// or outbox. The next generation changes the reviewed head as well.
			carrySecondReviewJob(t, f, state)
			if err := os.Remove(filepath.Join(f.Executor.Store.Root, "state", "job-1.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(f.Executor.Config.RuntimeDir, "control", "outbox")); err != nil {
				t.Fatal(err)
			}

			state, err = f.Executor.Execute(context.Background(), f.Job, func(context.Context) error { return nil })
			if err != nil || state.Status != "held" || reviews != 3 || state.HeadSHA == oldRecord.Head {
				t.Fatalf("next publish/hold: %+v err=%v reviews=%d", state, err, reviews)
			}
			bodies := proposalBodies(t, f)
			var first, second []followupProposalItem
			if len(bodies) != 2 || json.Unmarshal(bodies[0]["items"], &first) != nil || json.Unmarshal(bodies[1]["items"], &second) != nil || len(first) != 1 || first[0] != (followupProposalItem{Problem: "retained proposal", Expected: fixtureEvidence + "期待: retained proposal", Test: fixtureVerification + " 検査: retained proposal"}) || len(second) != 1 || second[0].Problem != "new ordinary improvement" {
				t.Fatalf("replayed retained item or lost new improvement: %v", bodies)
			}
			saved, err := f.Executor.Store.Load(1)
			var review Review
			_ = json.Unmarshal(saved.Review, &review)
			if err != nil || len(review.DeferredReviewItems) != 2 || review.DeferredReviewItems[1].ExternalCheckOnly == nil || !*review.DeferredReviewItems[1].ExternalCheckOnly || len(review.DeferredReviewDeliveries) != 2 || len(saved.FollowupProposals) != 2 || saved.FollowupProposals[0] != firstDelivery {
				t.Fatalf("delivery history/details not carried: %+v review=%+v err=%v", saved.FollowupProposals, review, err)
			}
			var final struct {
				FollowupProposals []journal.FollowupProposal `json:"followupProposals"`
			}
			if json.Unmarshal(state.Result, &final) != nil || !reflect.DeepEqual(final.FollowupProposals, saved.FollowupProposals) {
				t.Fatal("final delivery history lost", string(state.Result))
			}
		})
	}
}

func TestSecondReviewInterruptedSplitDelivery(t *testing.T) {
	f := newPipeline(t, "approve")
	r := proposalRecord()
	r.ReviewHolds = 1
	r.PreviousReview = secondReviewResponse("data", "previous must")
	r.Review = nil
	// Repeated equal 2000-unit parts still have distinct positions.
	text := strings.Repeat("x", 20000)
	r.PreviousReview = changeSecondReview(r.PreviousReview, "deferredReviewItems", []followupProposalItem{{Problem: text, Expected: fixtureEvidence + "期待: " + text, Test: fixtureVerification + " 検査: " + text}, {Problem: "根拠が未確認の持ち越し", Expected: "未確認", Test: "未確認"}})
	_, r.Review, _ = decideReview(r, encode(map[string]any{"decision": "approve"}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.server.Config.Handler.ServeHTTP(w, req)
		if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/comments") {
			cancel()
		}
	}))
	t.Cleanup(server.Close)
	client, err := gitea.New(server.URL, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	f.Executor.Gitea = client
	if err := f.Executor.postFollowupProposals(ctx, r); err == nil || ctx.Err() == nil || len(proposalBodies(t, f)) != 1 {
		t.Fatal("did not interrupt after first batch", err)
	}
	state, err := f.Executor.finish(r, "held", "permissions", "approval required", nil)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh PC receives the final review JSON, without the previous outbox.
	if err := os.RemoveAll(filepath.Join(f.Executor.Config.RuntimeDir, "control", "outbox")); err != nil {
		t.Fatal(err)
	}
	next := proposalRecord()
	next.Generation, next.Head, next.Review = 1, strings.Repeat("b", 40), nil
	seedPreviousResults(next, Descriptor{Descriptor: jobspec.Descriptor{Generation: 1, Result: state.Result}})
	_, next.Review, _ = decideReview(next, encode(map[string]any{"decision": "approve"}))
	f.Executor.Gitea, err = gitea.New(f.server.URL, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Executor.postFollowupProposals(context.Background(), next); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(next.Review), "根拠が未確認の持ち越し") {
		t.Fatal("ineligible retained finding was lost")
	}
	var all []followupProposalItem
	for _, body := range proposalBodies(t, f) {
		var items []followupProposalItem
		if err := json.Unmarshal(body["items"], &items); err != nil {
			t.Fatal(err)
		}
		all = append(all, items...)
	}
	want := proposalWireItems([]followupProposalItem{{Problem: text, Expected: fixtureEvidence + "期待: " + text, Test: fixtureVerification + " 検査: " + text}})
	if !reflect.DeepEqual(all, want) || len(proposalBodies(t, f)) != (len(want)+4)/5 || len(next.FollowupProposals) != (len(want)+4)/5 {
		t.Fatalf("split items lost/replayed: got=%d want=%d deliveries=%+v", len(all), len(want), next.FollowupProposals)
	}
}

func TestSecondReviewCarryAfterLocalDelivery(t *testing.T) {
	for _, path := range []string{"sent", "unknown", "ordinary across generations"} {
		t.Run(path, func(t *testing.T) {
			unknown := path == "unknown"
			f := newPipeline(t, "approve")
			f.DropNoticeResponse, f.HideNotices = unknown, unknown
			r := proposalRecord()
			r.Review = stampReviewEvidence(changeSecondReview(secondReviewResponse("other", "local improvement"), "findingSeverities", []string{"minor"}), 0, 3, r.Head)
			_, r.Review = normalizeReview(r.Review)
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			if path == "ordinary across generations" {
				// Ordinary improvements keep their per-generation contract. Both
				// attempts must survive a later retention merge at the new head.
				r.Generation++
				r.Head = strings.Repeat("b", 40)
				r.Review = changeSecondReview(changeSecondReview(r.Review, "reviewedGeneration", r.Generation), "reviewedHead", r.Head)
				if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
					t.Fatal(err)
				}
			}
			oldCount := len(r.FollowupProposals)
			oldID := r.FollowupProposals[oldCount-1].ID
			oldStatus := r.FollowupProposals[oldCount-1].Status
			if unknown && oldStatus != "failed" {
				t.Fatal("expected unknown first response", r.FollowupProposals)
			}
			f.DropNoticeResponse = false
			item := followupProposalItem{Problem: "carried finding", Expected: fixtureEvidence + "carried expected", Test: fixtureVerification + " carried test"}
			incoming := changeSecondReview(stampReviewEvidence(secondReviewResponse("data", "old must"), 0, 2, r.Head), "deferredReviewItems", []followupProposalItem{item})
			seedPreviousResults(r, Descriptor{Descriptor: jobspec.Descriptor{Generation: r.Generation, Result: encode(map[string]any{"review": incoming})}})
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			bodies := proposalBodies(t, f)
			var sent []followupProposalItem
			if len(bodies) != oldCount+1 || json.Unmarshal(bodies[oldCount]["items"], &sent) != nil || !reflect.DeepEqual(sent, []followupProposalItem{item}) || len(r.FollowupProposals) != oldCount+1 || r.FollowupProposals[oldCount-1].ID != oldID || r.FollowupProposals[oldCount-1].Status != oldStatus || r.FollowupProposals[oldCount].Status != "sent" || r.FollowupProposals[oldCount].ID == oldID {
				t.Fatalf("lost new carry or reposted old approval: bodies=%v results=%+v", bodies, r.FollowupProposals)
			}

			// Once the original marker becomes visible, recover the old result with
			// GET only. The newly carried item must keep its distinct, sent batch.
			f.HideNotices = false
			if err := f.Executor.postFollowupProposals(context.Background(), r); err != nil {
				t.Fatal(err)
			}
			saved, err := f.Executor.Store.Load(r.ID)
			if err != nil || len(proposalBodies(t, f)) != oldCount+1 || len(saved.FollowupProposals) != oldCount+1 || saved.FollowupProposals[oldCount-1].Status != "sent" || saved.FollowupProposals[oldCount].Status != "sent" {
				t.Fatal("delivery recovery not persisted", saved, err)
			}
		})
	}
}

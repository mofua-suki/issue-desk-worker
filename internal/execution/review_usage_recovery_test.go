package execution

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/providers"
)

func TestApprovedReviewSurvivesUsageDeliveryFailure(t *testing.T) {
	for _, target := range []string{"same", "head", "base", "request"} {
		t.Run(target, func(t *testing.T) {
			f := newPipeline(t, "approve")
			f.AllowUsage = true
			originalAPI := f.API
			reviews := 0
			originalCall := f.Executor.Call
			f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
				if req.Review {
					reviews++
				}
				return originalCall(ctx, req)
			}
			var failed, delivered json.RawMessage
			proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.URL.Path == "/api/v1/usage" && reviews == 1 && failed == nil {
					failed = append([]byte(nil), body...)
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if r.URL.Path == "/api/v1/usage" && failed != nil && delivered == nil {
					delivered = append([]byte(nil), body...)
				}
				response, err := originalAPI.Explicit(r.Context(), r.Method, r.URL.RequestURI(), body)
				if err != nil {
					t.Errorf("proxy: %v", err)
					w.WriteHeader(500)
					return
				}
				w.WriteHeader(response.Status)
				_, _ = w.Write(response.Body)
			}))
			defer proxy.Close()
			client, err := intake.New(configBootstrap(proxy.URL), "idk_"+strings.Repeat("a", 43), proxy.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			f.Executor.API = client
			ctx := context.Background()
			fence := func(context.Context) error { return nil }
			_, err = f.Executor.Execute(ctx, f.Job, fence)
			if err == nil || failed == nil || f.CreatePosts != 0 || reviews != 1 {
				t.Fatalf("first err=%v failed=%s PR=%d reviews=%d", err, failed, f.CreatePosts, reviews)
			}
			saved, err := f.Executor.Store.Load(1)
			if err != nil {
				t.Fatal(err)
			}
			switch target {
			case "head":
				f.git(saved.Tree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "new head")
			case "base":
				source := filepath.Join(f.root, "source")
				if err := os.WriteFile(filepath.Join(source, "upstream.txt"), []byte("upstream\n"), 0600); err != nil {
					t.Fatal(err)
				}
				f.git(source, "add", "upstream.txt")
				f.git(source, "commit", "-m", "upstream")
				f.git(source, "push", f.bare, "main")
			case "request":
				f.GiteaOverride = func(w http.ResponseWriter, r *http.Request) bool {
					if r.Method == "GET" && r.URL.Path == "/api/v1/repos/owner/project/issues/7" {
						io.WriteString(w, `{"number":7,"title":"Implement feature","body":"changed request","state":"open","user":{"login":"issue-desk-intake"}}`)
						return true
					}
					return false
				}
			}
			f.Job.LeaseEpoch++
			f.CurrentLeaseEpoch = f.Job.LeaseEpoch
			state, err := f.Executor.Execute(ctx, f.Job, fence)
			wantReviews := 2
			if target == "same" {
				wantReviews = 1
			}
			if err != nil || state.Status != "merged" || reviews != wantReviews || f.CreatePosts != 1 || string(failed) != string(delivered) {
				t.Fatalf("resume state=%+v err=%v reviews=%d PR=%d same usage=%v", state, err, reviews, f.CreatePosts, string(failed) == string(delivered))
			}
		})
	}
}

func TestReviewRecoveryRejectsHeadChangedAtFence(t *testing.T) {
	f := newPipeline(t, "approve")
	f.AllowUsage = true
	var reviewTree string
	original := f.Executor.Call
	f.Executor.Call = func(ctx context.Context, req providers.Request) (providers.Answer, error) {
		answer, err := original(ctx, req)
		if req.Review {
			reviewTree = req.Policy.Tree
		}
		return answer, err
	}
	changed := false
	_, err := f.Executor.Execute(context.Background(), f.Job, func(context.Context) error {
		if reviewTree != "" && !changed {
			changed = true
			f.git(reviewTree, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "changed during fence")
		}
		return nil
	})
	if !changed || err == nil || f.CreatePosts != 0 {
		t.Fatalf("changed=%v err=%v PR=%d", changed, err, f.CreatePosts)
	}
	saved, loadErr := f.Executor.Store.Load(1)
	if loadErr != nil || saved == nil || saved.ReviewUsageRecovery != nil {
		t.Fatalf("recovery saved after head change: record=%+v err=%v", saved, loadErr)
	}
}

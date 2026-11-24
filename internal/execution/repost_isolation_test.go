package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"issue-desk-worker/internal/journal"
)

func TestRepostErrorsDoNotHideJobCommands(t *testing.T) {
	for _, fault := range []string{"read503", "transport", "ack503", "notice503"} {
		t.Run(fault, func(t *testing.T) {
			f := newRepostFixture(t)
			store, err := journal.Open(f.e.Config.RuntimeDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			f.e.Store = store
			canceled := false
			active := &control{epoch: 3, cancel: func() { canceled = true }}
			f.e.controls.byID = map[int64]*control{9: active}
			for _, kind := range []string{"instruction", "stop"} {
				c := command{ID: kind + "-1", Kind: kind}
				c.Payload.JobID, c.Payload.Epoch, c.Payload.Text = 9, 3, "追加指示"
				f.commands = append(f.commands, c)
			}
			switch fault {
			case "read503":
				f.readStatus = 503
			case "transport":
				f.dropRead = true
			case "ack503":
				f.ackStatus = 503
			case "notice503":
				f.listStatus = 503
			}
			f.poll(t)
			if !canceled || active.reason != "stop" || len(active.ids) != 2 {
				t.Fatalf("job control lost: canceled=%v reason=%s ids=%v", canceled, active.reason, active.ids)
			}
			if fault != "ack503" && len(f.acks) != 0 {
				t.Fatal("transient failure was acked")
			}
			if fault == "ack503" {
				r, err := f.e.loadRepostResult(f.commands[0])
				if err != nil || r == nil || r.AckStatus != "" || f.posts != 1 {
					t.Fatal("failed ack was not left pending", err)
				}
			}
		})
	}
}

func readRepostNotice(t *testing.T, f *repostFixture, n notification) notification {
	t.Helper()
	raw, err := os.ReadFile(f.e.notificationFile(n))
	var saved notification
	if err != nil || json.Unmarshal(raw, &saved) != nil {
		t.Fatal("missing durable notification", err)
	}
	return saved
}

func TestRepostPermanentFailuresDoNotBlockRecover(t *testing.T) {
	for _, fault := range []string{"assignment", "account", "gitUser", "secret", "marker", "ack404", "ack503", "result-mismatch", "damaged-result"} {
		t.Run(fault, func(t *testing.T) {
			f := newRepostFixture(t)
			f.listStatus = 503
			f.poll(t) // Produce the real prepared outbox and result files.
			r, err := f.e.loadRepostResult(f.commands[0])
			if err != nil || r == nil || r.Notice == nil {
				t.Fatal("missing prepared result", err)
			}
			original := *r.Notice
			f.restart()
			store, err := journal.Open(f.e.Config.RuntimeDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			f.e.Store = store
			f.listStatus = 0
			normal := notification{Repo: "owner/project", Target: 7, Kind: "comment", Author: "worker", Marker: "<!-- ordinary -->", Body: "ordinary", Stage: "prepared"}
			if err := f.e.saveNotification(normal); err != nil {
				t.Fatal(err)
			}
			f.comments = []map[string]any{{"body": normal.Marker, "user": map[string]any{"login": "worker"}}}
			reason := ""
			switch fault {
			case "assignment":
				f.e.Config.Assignments.Implementers = nil
				reason = "repository_not_assigned"
			case "account":
				f.e.Config.Assignments.WorkerAccounts["fixture"] = "replacement"
				reason = "worker_account_changed"
			case "gitUser":
				f.e.Config.GitUser = "worker"
				reason = "worker_account_is_git_user"
			case "secret":
				f.e.secret = "proposal_1"
				reason = "saved_repost_body_unsafe"
			case "marker":
				f.comments = append(f.comments, map[string]any{"body": original.Marker, "user": map[string]any{"login": "third-party"}})
				reason = "marker_author"
			case "ack404", "ack503", "damaged-result":
				f.comments = append(f.comments, map[string]any{"body": original.Body, "user": map[string]any{"login": "worker"}})
				f.ackStatus = 404
				if fault == "ack503" {
					f.ackStatus = 503
				}
				if fault == "damaged-result" {
					if err := os.WriteFile(f.e.repostResultFile(r.Command.ID), []byte("{"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "result-mismatch":
				f.commands[0].Payload.ProposalID = encode("different")
				f.poll(t)
				reason = "saved_repost_mismatch"
			}
			for i := 0; i < 2; i++ {
				if err := f.e.Recover(context.Background()); err != nil {
					t.Fatalf("startup recovery %d blocked: %v", i, err)
				}
				// Exercise the actual request entry too, with its scheduler interval
				// pending so that no unrelated request needs to be manufactured.
				f.e.requestsNext = time.Now().Add(time.Hour)
				if err := f.e.ProcessRequestsFor(context.Background(), []string{"owner/project"}); err != nil {
					t.Fatalf("request recovery blocked: %v", err)
				}
			}
			if f.posts != 0 || readRepostNotice(t, f, normal).Stage != "sent" {
				t.Fatal("repost duplicated or ordinary outbox did not recover", f.posts)
			}
			if reason != "" {
				n := readRepostNotice(t, f, original)
				if n.Stage != "refused" || n.RefusalReason != reason {
					t.Fatalf("refusal not durable: %+v", n)
				}
				f.last(t, "failed", "refused", reason)
			}
			if fault == "ack404" {
				r, err := f.e.loadRepostResult(f.commands[0])
				if err != nil || r.AckRejected != 404 || r.AckStatus != "" || len(f.acks) != 1 {
					t.Fatal("permanent ack refusal not terminal", r, err)
				}
				f.poll(t)
				if len(f.acks) != 1 {
					t.Fatal("permanently rejected ack retried on redelivery")
				}
			}
			if fault == "ack503" {
				r, err := f.e.loadRepostResult(f.commands[0])
				if err != nil || r.AckRejected != 0 || r.AckStatus != "" {
					t.Fatal("transient ack was finalized", r, err)
				}
				f.ackStatus = 0
				if err := f.e.Recover(context.Background()); err != nil {
					t.Fatal(err)
				}
				f.last(t, "done", "queued", "")
			}
		})
	}
}

func TestRepostCommentIssueUsesPathIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, issueURL, pullURL string
		ok                      bool
	}{
		{"issue-html", "https://git.example/Owner/Project/issues/7", "", true},
		{"pr-html", "", "https://git.example/owner/project/pulls/7", true},
		{"issue-subpath", "http://git.example:8123/gitea/OWNER/PROJECT/issues/007", "", true},
		{"pr-subpath", "", "https://git.example/sub/owner/project/pulls/7", true},
		{"issue-api", "https://git.example/api/v1/repos/Owner/Project/issues/7", "", true},
		{"pr-api-subpath", "", "https://git.example/sub/api/v1/repos/owner/project/pulls/7", true},
		{"issue-preferred", "https://git.example/owner/project/issues/7", "https://git.example/owner/other/pulls/8", true},
		{"issue-number", "https://git.example/owner/project/issues/8", "", false},
		{"pr-number", "", "https://git.example/sub/owner/project/pulls/8", false},
		{"issue-repo", "https://git.example/owner/other/issues/7", "", false},
		{"pr-repo", "", "https://git.example/sub/other/project/pulls/7", false},
		{"issue-kind", "https://git.example/owner/project/releases/7", "", false},
		{"pr-kind", "", "https://git.example/owner/project/releases/7", false},
		{"both-empty", "", "", false},
		{"no-fallback-for-wrong-issue", "https://git.example/owner/project/issues/8", "https://git.example/owner/project/pulls/7", false},
		{"extra-segment", "https://git.example/owner/project/issues/7/extra", "", false},
		{"number-overflow", "https://git.example/owner/project/issues/9223372036854775808", "", false},
		{"invalid-url", "garbage", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepostFixture(t)
			f.comment["issue_url"] = tc.issueURL
			f.comment["pull_request_url"] = tc.pullURL
			f.comment["user"] = map[string]any{"login": "GIT-USER"}
			f.poll(t)
			if tc.ok {
				f.last(t, "done", "queued", "")
				if f.posts != 1 {
					t.Fatal("valid ROOT_URL variant refused")
				}
			} else {
				f.last(t, "failed", "refused", "comment_issue")
				if f.posts != 0 {
					t.Fatal("foreign comment reposted")
				}
			}
		})
	}
}

func TestRepostAck4xxRemainsTerminalAfterRestart(t *testing.T) {
	for _, status := range []int{400, 404, 409, 410, 413, 422} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newRepostFixture(t)
			f.commands[0].Payload.V = encode(2) // Refusal without any outbox.
			f.ackStatus = status
			f.poll(t)
			f.restart()
			for i := 0; i < 2; i++ {
				if err := f.e.recoverNotifications(context.Background()); err != nil {
					t.Fatal(err)
				}
				if status != 409 {
					f.poll(t)
				}
			}
			r, err := f.e.loadRepostResult(f.commands[0])
			if err != nil || r == nil || len(f.acks) != 1 || f.posts != 0 {
				t.Fatal("ack refusal blocked recovery or retried", err)
			}
			if status == 409 {
				if r.AckStatus != "failed" || r.AckRejected != 0 {
					t.Fatal("409 completion was not preserved")
				}
			} else if r.AckRejected != status || r.AckStatus != "" {
				t.Fatal("ack refusal was confused with acceptance")
			}
		})
	}
}

func TestRepostTransientAck4xxRetriesAfterRestart(t *testing.T) {
	for _, status := range []int{401, 403, 408, 429} {
		for _, refused := range []bool{false, true} {
			for _, recovery := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/refused=%t/recover=%t", status, refused, recovery), func(t *testing.T) {
					f := newRepostFixture(t)
					wantStatus, wantResult, wantReason, wantPosts := "done", "queued", "", 1
					if refused {
						f.commands[0].Payload.V = encode(2)
						wantStatus, wantResult, wantReason, wantPosts = "failed", "refused", "payload_version", 0
					}
					store, err := journal.Open(f.e.Config.RuntimeDir)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { store.Close() })
					f.ackStatus = status
					f.poll(t)
					for attempt := 1; attempt <= 3; attempt++ {
						r, err := f.e.loadRepostResult(f.commands[0])
						if err != nil || r == nil {
							t.Fatal("missing saved result", err)
						}
						wantAck := ""
						if attempt == 3 {
							wantAck = wantStatus
						}
						if r.AckRejected != 0 || r.AckStatus != wantAck || len(f.acks) != attempt || f.posts != wantPosts {
							t.Fatalf("attempt %d: result=%+v acks=%d posts=%d", attempt, r, len(f.acks), f.posts)
						}
						f.last(t, wantStatus, wantResult, wantReason)
						if !refused && (r.Result.ProposalID != "proposal_1" || r.Result.CommentID != 42 || readRepostNotice(t, f, *r.Notice).Stage != "sent") {
							t.Fatal("queued result or delivered outbox changed during ack retry")
						}
						if attempt == 3 {
							break
						}
						// A still-transient response after restart remains pending;
						// the following restart sees the recovered intake service.
						if attempt == 2 {
							f.ackStatus = 0
						}
						f.restart()
						if recovery {
							f.e.Store = store
							if err := f.e.Recover(context.Background()); err != nil {
								t.Fatal("ack failure blocked recovery", err)
							}
						} else {
							f.poll(t)
						}
					}
				})
			}
		}
	}
}

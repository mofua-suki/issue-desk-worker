package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type repostFixture struct {
	wireCommands any
	e            *Executor
	commands     []command
	comment      map[string]any
	comments     []map[string]any
	acks         []struct {
		Status string        `json:"status"`
		Result repostOutcome `json:"result"`
	}
	readStatus, postStatus, ackStatus, listStatus int
	readCalls, posts                              int
	drop, hide, dropRead                          bool
	dropReadMu                                    sync.Mutex
	posted                                        string
	g                                             *httptest.Server
}

func newRepostFixture(t *testing.T) *repostFixture {
	t.Helper()
	f := &repostFixture{}
	c := command{ID: "repost-1", Kind: "repost_proposal", Repo: "owner/project", Issue: encode(7), TargetHost: "fixture"}
	c.Payload.V = encode(1)
	c.Payload.CommentID = encode(42)
	c.Payload.ProposalID = encode("proposal_1")
	f.commands = []command{c}
	f.comment = map[string]any{"id": 42, "body": proposalText(proposalFixture(1)) + "\n<!-- gcr:original -->", "user": map[string]any{"login": "git-user"}}
	f.g = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token fixture-worker-token" {
			t.Error("wrong posting client")
		}
		switch {
		case r.URL.Path == "/api/v1/repos/owner/project/issues/comments/42":
			f.readCalls++
			f.dropReadMu.Lock()
			dropRead := f.dropRead
			f.dropReadMu.Unlock()
			if dropRead {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			if f.readStatus != 0 {
				w.WriteHeader(f.readStatus)
				return
			}
			json.NewEncoder(w).Encode(f.comment)
		case r.URL.Path == "/api/v1/repos/owner/project/issues/7/comments" && r.Method == "GET":
			if f.listStatus != 0 {
				w.WriteHeader(f.listStatus)
				return
			}
			if f.hide {
				w.Write([]byte(`[]`))
			} else {
				json.NewEncoder(w).Encode(f.comments)
			}
		case r.URL.Path == "/api/v1/repos/owner/project/issues/7/comments" && r.Method == "POST":
			f.posts++
			var body map[string]string
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				t.Error("bad POST body")
			}
			f.posted = body["body"]
			// The production outbox must be durable before every POST, even if a
			// crash happened before the separate command result was saved.
			nkey := notification{Repo: "owner/project", Marker: "<!-- issue-desk-repost:42 -->"}
			raw, err := os.ReadFile(f.e.notificationFile(nkey))
			var n notification
			if err != nil || json.Unmarshal(raw, &n) != nil || n.Stage != "attempted" {
				t.Error("attempt not durable")
			}

			if f.postStatus != 0 {
				w.WriteHeader(f.postStatus)
				return
			}
			f.comments = append(f.comments, map[string]any{"body": f.posted, "user": map[string]any{"login": "worker"}})
			if f.drop {
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
				return
			}
			w.Write([]byte(`{"id":99}`))
		default:
			t.Errorf("unexpected Gitea route %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.g.Close)
	f.comment["issue_url"] = f.g.URL + "/owner/project/issues/7"
	f.comment["pull_request_url"] = ""
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/commands":
			if r.URL.Query().Get("host") != "fixture" || r.URL.Query().Get("after") != "0" {
				t.Error("wrong list query")
			}
			rows := any(f.commands)
			if f.wireCommands != nil {
				rows = f.wireCommands
			}
			json.NewEncoder(w).Encode(map[string]any{"commands": rows, "next": 100})
		case strings.HasSuffix(r.URL.Path, "/ack"):
			var ack struct {
				Status string        `json:"status"`
				Result repostOutcome `json:"result"`
			}
			if json.NewDecoder(r.Body).Decode(&ack) != nil {
				t.Error("bad ack")
			}
			if ack.Result.Result == "" || ack.Status == "acked" {
				t.Error("missing terminal result")
			}
			f.acks = append(f.acks, ack)
			entries, _ := os.ReadDir(filepath.Join(f.e.Config.RuntimeDir, "control", "reposts"))
			if len(entries) == 0 {
				t.Error("ack before durable result")
			}
			if f.ackStatus != 0 {
				w.WriteHeader(f.ackStatus)
				return
			}
			w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected intake route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	api, err := intake.New(configBootstrap(server.URL), "idk_"+strings.Repeat("a", 43), server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gitea.New(f.g.URL, "fixture-worker-token")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Configuration{Host: "fixture", Fleet: Fleet{RuntimeDir: t.TempDir(), GitUser: "git-user", Repositories: []Repository{{Name: "owner/project"}}}}
	cfg.Assignments.WorkerAccounts = map[string]string{"fixture": "worker"}
	cfg.Assignments.Implementers = map[string][]string{"owner/project": {"fixture"}}
	f.e = &Executor{Config: cfg, API: api, Gitea: g}
	return f
}
func (f *repostFixture) restart() {
	f.e = &Executor{Config: f.e.Config, API: f.e.API, Gitea: f.e.Gitea, secret: f.e.secret}
}
func (f *repostFixture) poll(t *testing.T) {
	t.Helper()
	if err := f.e.pollCommands(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func (f *repostFixture) last(t *testing.T, status, result, reason string) {
	t.Helper()
	if len(f.acks) == 0 {
		t.Fatal("no ack")
	}
	a := f.acks[len(f.acks)-1]
	if a.Status != status || a.Result.Result != result || a.Result.Reason != reason {
		t.Fatalf("ack %+v", a)
	}
}

func TestRepostAllVersionsAndDurableDeduplication(t *testing.T) {
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			f := newRepostFixture(t)
			original := proposalText(proposalFixture(version)) + "\n<!-- gcr:old -->"
			f.comment["body"] = original
			f.poll(t)
			f.last(t, "done", "queued", "")
			if f.posts != 1 || f.posted != repostBody(original, 42) {
				t.Fatal("wrong body/posts")
			}
			if _, err := parseProposalComment(f.posted); err != nil {
				t.Fatal(err)
			}
			a := f.acks[0]
			if a.Result.ProposalID != "proposal_1" || a.Result.CommentID != 42 {
				t.Fatal(a)
			}
			// Changes to the original after queuing cannot replace the durable proposal.
			f.comment["body"] = "edited later"
			f.poll(t)
			f.restart()
			f.poll(t)
			if err := f.e.recoverNotifications(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.commands[0].ID = "repost-2"
			f.poll(t)
			if f.posts != 1 || f.readCalls != 1 {
				t.Fatalf("duplicate %d reads %d", f.posts, f.readCalls)
			}
		})
	}
}
func TestRepostCommandAndCommentRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*repostFixture)
		reason string
	}{
		{"target", func(f *repostFixture) { f.commands[0].TargetHost = "other" }, "target_host"},
		{"repo", func(f *repostFixture) { f.commands[0].Repo = "other/project" }, "repository_not_assigned"},
		{"repo-not-implementer", func(f *repostFixture) { f.e.Config.Assignments.Implementers = nil }, "repository_not_assigned"},
		{"repo-not-configured", func(f *repostFixture) { f.e.Config.Repositories = nil }, "repository_not_assigned"},
		{"account-missing", func(f *repostFixture) { f.e.Config.Assignments.WorkerAccounts = nil }, "worker_account_missing"},
		{"same-git-user", func(f *repostFixture) { f.e.Config.GitUser = "WORKER" }, "worker_account_is_git_user"},
		{"issue-zero", func(f *repostFixture) { f.commands[0].Issue = encode(0) }, "issue_number"},
		{"issue-string", func(f *repostFixture) { f.commands[0].Issue = encode("7") }, "issue_number"},
		{"issue-fraction", func(f *repostFixture) { f.commands[0].Issue = encode(7.5) }, "issue_number"},
		{"issue-over", func(f *repostFixture) { f.commands[0].Issue = encode(int64(9007199254740992)) }, "issue_number"},
		{"v", func(f *repostFixture) { f.commands[0].Payload.V = encode(2) }, "payload_version"},
		{"v-string", func(f *repostFixture) { f.commands[0].Payload.V = encode("1") }, "payload_version"},
		{"comment-negative", func(f *repostFixture) { f.commands[0].Payload.CommentID = encode(-1) }, "comment_id"},
		{"comment-fraction", func(f *repostFixture) { f.commands[0].Payload.CommentID = encode(42.5) }, "comment_id"},
		{"comment-type", func(f *repostFixture) { f.commands[0].Payload.CommentID = encode("42") }, "comment_id"},
		{"proposal-empty", func(f *repostFixture) { f.commands[0].Payload.ProposalID = encode("") }, "proposal_id"},
		{"proposal-type", func(f *repostFixture) { f.commands[0].Payload.ProposalID = encode(42) }, "proposal_id"},
		{"wrong-issue", func(f *repostFixture) { f.comment["issue_url"] = f.g.URL + "/owner/project/issues/8" }, "comment_issue"},
		{"wrong-repo", func(f *repostFixture) { f.comment["issue_url"] = f.g.URL + "/other/project/issues/7" }, "comment_issue"},
		{"wrong-comment-id", func(f *repostFixture) { f.comment["id"] = 43 }, "comment_issue"},
		{"wrong-author", func(f *repostFixture) { f.comment["user"] = map[string]any{"login": "worker"} }, "comment_author"},
		{"invalid-proposal", func(f *repostFixture) { f.comment["body"] = "not a proposal" }, "proposal_marker_count"},
		{"wrong-proposal", func(f *repostFixture) { f.commands[0].Payload.ProposalID = encode("different") }, "proposal_id_mismatch"},
		{"wrong-source", func(f *repostFixture) {
			p := proposalFixture(1)
			p["source"] = "owner/project#8"
			f.comment["body"] = proposalText(p)
		}, "proposal_source_mismatch"},
		{"redact-pattern", func(f *repostFixture) {
			f.comment["body"] = proposalText(proposalFixture(1)) + "\nsk-" + strings.Repeat("a", 25)
		}, "body_redacted"},
		{"redact-active-secret", func(f *repostFixture) {
			f.e.secret = "fixture-sensitive"
			f.comment["body"] = proposalText(proposalFixture(1)) + "\nfixture-sensitive"
		}, "body_redacted"},
		{"body-over", func(f *repostFixture) {
			f.comment["body"] = proposalText(proposalFixture(1)) + strings.Repeat("😀", 28000)
		}, "body_too_long"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepostFixture(t)
			tc.mutate(f)
			f.poll(t)
			f.last(t, "failed", "refused", tc.reason)
			f.restart()
			f.poll(t)
			if f.posts != 0 {
				t.Fatal("posted refused proposal")
			}
		})
	}
}
func TestRepostReadErrorsAreClassified(t *testing.T) {
	for _, status := range []int{400, 403, 404, 408, 429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newRepostFixture(t)
			f.readStatus = status
			err := f.e.pollCommands(context.Background())
			if status < 500 {
				if err != nil {
					t.Fatal(err)
				}
				f.last(t, "failed", "refused", "comment_read_4xx")
			} else {
				if err != nil || len(f.acks) != 0 {
					t.Fatal("transient read acked")
				}
				f.readStatus = 0
				f.poll(t)
				f.last(t, "done", "queued", "")
			}
		})
	}
	t.Run("transport", func(t *testing.T) {
		f := newRepostFixture(t)
		f.dropReadMu.Lock()
		f.dropRead = true
		f.dropReadMu.Unlock()
		if f.e.pollCommands(context.Background()) != nil || len(f.acks) > 0 {
			t.Fatal("transport error acked")
		}
		f.dropReadMu.Lock()
		f.dropRead = false
		f.dropReadMu.Unlock()
		f.poll(t)
	})
}
func TestRepostAckFailureAndUnknownPostSurviveRestart(t *testing.T) {
	f := newRepostFixture(t)
	f.drop = true
	f.hide = true
	f.ackStatus = 503
	f.poll(t) // Ack failures remain pending without failing job control.
	if f.posts != 1 {
		t.Fatal(f.posts)
	}
	f.restart()
	f.ackStatus = 0
	f.poll(t)
	f.last(t, "done", "queued", "")
	if err := f.e.recoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.posts != 1 {
		t.Fatal("unknown POST was resent")
	}
	f.hide = false
	if err := f.e.recoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ := f.e.loadRepostResult(f.commands[0])
	raw, _ := os.ReadFile(f.e.notificationFile(*r.Notice))
	var n notification
	json.Unmarshal(raw, &n)
	if n.Stage != "sent" || f.posts != 1 {
		t.Fatal(n.Stage, f.posts)
	}
}
func TestRepostPost4xxStopsOnlyReposts(t *testing.T) {
	for _, status := range []int{400, 403, 422, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newRepostFixture(t)
			f.postStatus = status
			f.ackStatus = 503
			f.poll(t) // A failed ack must not escape into job control.
			f.last(t, "failed", "refused", "post_4xx")
			f.restart()
			f.postStatus = 0
			f.ackStatus = 0
			if err := f.e.recoverNotifications(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.poll(t)
			f.commands[0].ID = "second-command"
			f.poll(t)
			if f.posts != 1 {
				t.Fatal("resent refused POST", f.posts)
			}
			r, _ := f.e.loadRepostResult(f.commands[0])
			raw, _ := os.ReadFile(f.e.notificationFile(*r.Notice))
			var n notification
			json.Unmarshal(raw, &n)
			if n.Stage != "refused" {
				t.Fatal(n.Stage)
			}
		})
	}
}
func TestRepostPostTimeoutAnd5xxNeverResend(t *testing.T) {
	for _, status := range []int{408, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newRepostFixture(t)
			f.postStatus = status
			f.poll(t)
			f.last(t, "done", "queued", "")
			f.restart()
			f.postStatus = 0
			f.poll(t)
			if err := f.e.recoverNotifications(context.Background()); err != nil {
				t.Fatal(err)
			}
			if f.posts != 1 {
				t.Fatal("resent uncertain POST")
			}
		})
	}
}
func TestRepostPreparedRecoveryCanReportLaterRefusal(t *testing.T) {
	f := newRepostFixture(t)
	f.listStatus = 503
	f.poll(t) // Reconciliation failure leaves a durable prepared notice.
	if f.posts != 0 || len(f.acks) != 0 {
		t.Fatal("sent before reconciliation")
	}
	f.commands = nil
	f.restart()
	f.listStatus = 0
	f.postStatus = 403
	// Keep the fixture's durable-boundary assertion independent of server redelivery.
	rEntries, _ := os.ReadDir(filepath.Join(f.e.Config.RuntimeDir, "control", "reposts"))
	raw, _ := os.ReadFile(filepath.Join(f.e.Config.RuntimeDir, "control", "reposts", rEntries[0].Name()))
	var r repostResult
	json.Unmarshal(raw, &r)
	f.commands = []command{r.Command}
	if err := f.e.recoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.last(t, "failed", "refused", "post_4xx")
	f.restart()
	if err := f.e.recoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.posts != 1 {
		t.Fatal("refused recovery reposted")
	}
}
func TestRepostOnlyTrustsWorkerMarkerAndRechecksAssignment(t *testing.T) {
	for _, author := range []string{"git-user", "other"} {
		t.Run(author, func(t *testing.T) {
			f := newRepostFixture(t)
			f.comments = []map[string]any{{"body": "<!-- issue-desk-repost:42 -->", "user": map[string]any{"login": author}}}
			if f.e.pollCommands(context.Background()) != nil || f.posts != 0 {
				t.Fatal("trusted another author")
			}
			f.last(t, "failed", "refused", "marker_author")
		})
	}
	t.Run("worker", func(t *testing.T) {
		f := newRepostFixture(t)
		f.comments = []map[string]any{{"body": "<!-- issue-desk-repost:42 -->", "user": map[string]any{"login": "worker"}}}
		f.poll(t)
		if f.posts != 0 {
			t.Fatal("ignored own marker")
		}
	})
	t.Run("assignment-changed", func(t *testing.T) {
		f := newRepostFixture(t)
		f.listStatus = 503
		_ = f.e.pollCommands(context.Background())
		f.restart()
		f.listStatus = 0
		f.e.Config.Assignments.Implementers = nil
		if f.e.recoverNotifications(context.Background()) != nil || f.posts != 0 {
			t.Fatal("sent after assignment lost")
		}
	})
}
func TestRepostBodyLengthBoundary(t *testing.T) {
	for _, extra := range []int{0, 1} {
		t.Run(fmt.Sprint(extra), func(t *testing.T) {
			f := newRepostFixture(t)
			base := proposalText(proposalFixture(1))
			suffix := "\n\n<!-- issue-desk-repost:42 -->"
			f.comment["body"] = base + strings.Repeat("a", 55000-utf16Length(base)-utf16Length(suffix)+extra)
			f.poll(t)
			if extra == 0 {
				f.last(t, "done", "queued", "")
				if utf16Length(f.posted) != 55000 {
					t.Fatal("wrong boundary")
				}
			} else {
				f.last(t, "failed", "refused", "body_too_long")
				if f.posts != 0 {
					t.Fatal("posted oversized")
				}
			}
		})
	}
}

func TestRepostCrashBetweenOutboxAndCommandResult(t *testing.T) {
	f := newRepostFixture(t)
	f.listStatus = 503
	f.poll(t) // The prepared outbox survives reconciliation failure.
	if err := os.Remove(f.e.repostResultFile(f.commands[0].ID)); err != nil {
		t.Fatal(err)
	}
	f.restart()
	f.listStatus = 0
	if err := f.e.recoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.poll(t)
	f.last(t, "done", "queued", "")
	if f.posts != 1 || f.readCalls != 1 {
		t.Fatal("recovered outbox duplicated or reread original")
	}
}
func TestRepostAckConflictAndRefusalRecoveryWithoutOutbox(t *testing.T) {
	f := newRepostFixture(t)
	f.commands[0].Payload.V = encode(2)
	f.ackStatus = 503
	f.poll(t) // The failed ack is saved for recovery.
	f.restart()
	f.ackStatus = 409
	if err := f.e.recoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.last(t, "failed", "refused", "payload_version")
	if f.posts != 0 {
		t.Fatal("posted refused command")
	}
	n := len(f.acks)
	if err := f.e.recoverNotifications(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.acks) != n {
		t.Fatal("retried finalized ack")
	}
}
func TestRepostJSONIntegerRepresentations(t *testing.T) {
	f := newRepostFixture(t)
	f.commands[0].Issue = json.RawMessage(`7.0`)
	f.commands[0].Payload.V = json.RawMessage(`1e0`)
	f.commands[0].Payload.CommentID = json.RawMessage(`4.2e1`)
	body := proposalText(proposalFixture(1))
	f.comment["body"] = strings.Replace(body, `"version":1`, `"version":1.0`, 1)
	f.poll(t)
	f.last(t, "done", "queued", "")
}

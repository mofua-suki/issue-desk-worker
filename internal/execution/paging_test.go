package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/gitea"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGiteaFiftyClampedPagesAcrossEveryPagedCaller(t *testing.T) {
	calls := 0
	head := strings.Repeat("a", 40)
	marker := "<!-- issue-desk-pr-request:123-abcdefab -->"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "GET" || r.URL.Query().Get("limit") != "50" {
			t.Errorf("旧Gitea最大50件契約と違います: %s", r.URL)
			w.WriteHeader(400)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		rows := []map[string]any{}
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			count := 50
			if page == 2 {
				count = 1
			}
			for n := 0; n < count; n++ {
				body := "other"
				if page == 2 {
					body = "review-marker"
				}
				rows = append(rows, map[string]any{"body": body, "commit_id": head, "user": map[string]string{"login": "worker"}})
			}
		} else {
			count := 50
			if page == 2 {
				count = 1
			}
			for n := 0; n < count; n++ {
				branch := fmt.Sprintf("other-%d-%d", page, n)
				if page == 2 {
					branch = "chat/work"
				}
				rows = append(rows, map[string]any{"number": int64(page*100 + n), "state": "open", "body": marker, "head": map[string]any{"ref": branch, "sha": head, "repo": map[string]string{"full_name": "o/r"}}, "base": map[string]any{"ref": "main", "repo": map[string]string{"full_name": "o/r"}}})
			}
		}
		json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	g, err := gitea.New(server.URL, "synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{Gitea: g}
	id, err := e.findOpenPull(context.Background(), "o/r", "chat/work", "main", head, false)
	if err != nil || id != 200 || calls != 2 {
		t.Fatalf("PR採用: %d %v %d", id, err, calls)
	}
	calls = 0
	p, err := e.findRequestPull(context.Background(), monitorRequest{ID: "123-abcdefab", Repo: "o/r"}, "chat/work")
	if err != nil || p == nil || p.Number != 200 || calls != 2 {
		t.Fatalf("直接PRmarker: %+v %v %d", p, err, calls)
	}
	calls = 0
	found, err := e.noticeFound(context.Background(), notification{Kind: "review", Repo: "o/r", Target: 7, Author: "worker", Head: head, Marker: "review-marker"})
	if err != nil || !found || calls != 2 {
		t.Fatalf("reviewmarker: %v %v %d", found, err, calls)
	}
}

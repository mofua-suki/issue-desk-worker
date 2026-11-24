package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/gitea"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// MAX_RESPONSE_ITEMS が50より小さいGiteaでは、50件未満のページでも最後とは限らない。X-Total-Count が残りを示す間は次のページを読む。
func TestPullListingFollowsTotalCountPastShortPages(t *testing.T) {
	const capped, total = 30, 31
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		rows := []map[string]any{}
		for n := (page - 1) * capped; n < min(page*capped, total); n++ {
			branch := fmt.Sprintf("other-%d", n)
			if n == total-1 {
				branch = "chat/work"
			}
			rows = append(rows, map[string]any{"number": int64(n + 1), "state": "open", "head": map[string]any{"ref": branch, "repo": map[string]string{"full_name": "o/r"}}, "base": map[string]any{"ref": "main", "repo": map[string]string{"full_name": "o/r"}}})
		}
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	g, err := gitea.New(server.URL, "synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{Gitea: g}
	p, err := e.findOpenBranchPull(context.Background(), "o/r", "chat/work", "main")
	if err != nil || p == nil || p.Number != total || calls != 2 {
		t.Fatalf("2ページ目のPRを見落としました: %+v %v %d", p, err, calls)
	}
	// 件数に届いたら、空のページを読みに行かない。
	calls = 0
	p, err = e.findOpenBranchPull(context.Background(), "o/r", "chat/missing", "main")
	if err != nil || p != nil || calls != 2 {
		t.Fatalf("件数に届いた後も読みました: %+v %v %d", p, err, calls)
	}
}

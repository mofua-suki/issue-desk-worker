package gitea

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 応答の失敗は、呼んだメソッドとパスの形（番号・クエリを除く）を示し、上限超えを壊れたJSONと分ける。
func TestResponseErrorsNameCallAndLimit(t *testing.T) {
	big := bytes.Repeat([]byte(" "), maxResponseBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files") {
			w.Write(big)
			return
		}
		w.Write([]byte("{broken"))
	}))
	defer server.Close()
	c, err := New(server.URL, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	err = c.Request(context.Background(), "GET", "/repos/owner/repo/pulls/12/files?page=3", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "上限") || !strings.Contains(err.Error(), "GET /repos/owner/repo/pulls/{n}/files") || strings.Contains(err.Error(), "page") {
		t.Fatalf("上限超えが分かりません: %v", err)
	}
	err = c.Request(context.Background(), "POST", "/repos/owner/repo/pulls/7/reviews", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "JSON") || !strings.Contains(err.Error(), "POST /repos/owner/repo/pulls/{n}/reviews") {
		t.Fatalf("壊れたJSONの呼び出しが分かりません: %v", err)
	}
}

func TestPathTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"/repos/o/r/pulls/12":           "/repos/o/r/pulls/{n}",
		"/repos/o/r/pulls/12/reviews/3": "/repos/o/r/pulls/{n}/reviews/{n}",
		"/repos/o/r/pulls?state=open":   "/repos/o/r/pulls",
		"/repos/o/r2d2/pulls/1/files":   "/repos/o/r2d2/pulls/{n}/files",
	} {
		if got := pathTemplate(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

package execution

import (
	"context"
	"io"
	"issue-desk-worker/internal/gitea"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNonDestructiveWorkerScopeProbes(t *testing.T) {
	for _, status := range []int{404, 422, 401, 403, 200, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method == "GET" && r.URL.Path == "/api/v1/repos/a/b" {
					w.Write([]byte(`{}`))
					return
				}
				if r.Method != "PATCH" || !(r.URL.Path == "/api/v1/repos/a/b/issues/0" || r.URL.Path == "/api/v1/repos/a/b/pulls/0") {
					t.Errorf("危険なprobe: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			c := Configuration{Host: "test"}
			c.Assignments.Implementers = map[string][]string{"a/b": {"test"}}
			g, err := gitea.New(server.URL, "synthetic-secret")
			if err != nil {
				t.Fatal(err)
			}
			err = verifyWorkerScopes(context.Background(), c, g)
			if (err == nil) != (status == 404 || status == 422) {
				t.Fatalf("status=%d err=%v", status, err)
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-secret") {
				t.Fatal("秘密値を含めました")
			}
			if status == 404 || status == 422 {
				if calls != 3 {
					t.Fatal(calls)
				}
			}
		})
	}
}

// 権限の検査の失敗は、対象のリポジトリとGiteaの応答の状態を示す。番号0の更新が通ったときも区別する。
func TestWorkerScopeErrorsNameRepositoryAndStatus(t *testing.T) {
	for name, tc := range map[string]struct {
		read, write int
		want        []string
	}{
		"read":  {401, 404, []string{"owner/project", "HTTP 401"}},
		"write": {200, 200, []string{"owner/project", "番号0の更新が成功しました"}},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(tc.read)
				} else {
					w.WriteHeader(tc.write)
				}
				io.WriteString(w, `{}`)
			}))
			defer server.Close()
			g, err := gitea.New(server.URL, "fixture-token")
			if err != nil {
				t.Fatal(err)
			}
			c := Configuration{Host: "fixture"}
			c.Assignments.Implementers = map[string][]string{"owner/project": {"fixture"}}
			err = verifyWorkerScopes(context.Background(), c, g)
			for _, want := range tc.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%sがありません: %v", want, err)
				}
			}
		})
	}
}

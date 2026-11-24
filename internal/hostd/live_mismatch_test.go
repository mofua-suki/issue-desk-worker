package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"issue-desk-worker/internal/process"
)

// 実地確認は、受付の依頼が手元の設定と時間・PCで一致しなければ実行せず not_allowed を送る。
// 実行した確認が出力の上限を超えたら output_limit、時間切れなら timeout を送る。
func TestLiveCheckReportsMismatchAndRunFailures(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for name, tc := range map[string]struct {
		timeout int
		host    string
		run     func() (process.Result, error)
		reason  string
		runs    int
	}{
		"時間が短い":  {timeout: 9, host: "test", reason: "not_allowed"},
		"時間の不一致": {timeout: 11, host: "test", reason: "not_allowed"},
		"PCの不一致": {timeout: 10, host: "other", reason: "not_allowed"},
		"出力の上限":  {timeout: 10, host: "test", run: func() (process.Result, error) { return process.Result{Started: true}, process.ErrOutputLimit }, reason: "output_limit", runs: 1},
		"時間切れ": {timeout: 10, host: "test", run: func() (process.Result, error) {
			return process.Result{Started: true, Interrupted: true}, errors.New("killed")
		}, reason: "timeout", runs: 1},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			var mu sync.Mutex
			var reasons []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch {
				case strings.HasSuffix(req.URL.Path, "live-check-next"):
					json.NewEncoder(w).Encode(map[string]any{"job": map[string]any{"id": 7, "repo": "owner/project", "commit": commit, "host": tc.host, "command": []string{"approved"}, "timeoutSeconds": tc.timeout}})
				case strings.HasSuffix(req.URL.Path, "live-check-result"):
					var body livePending
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Result.Pass || len(body.Result.Failures) != 1 {
						t.Errorf("結果が不正です: %+v %v", body, err)
					} else {
						mu.Lock()
						reasons = append(reasons, body.Result.Failures[0].Reason)
						mu.Unlock()
					}
					w.Write([]byte("{}"))
				default:
					t.Error("unexpected endpoint", req.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			runs := 0
			r := &Runtime{Config: Configuration{URL: server.URL, Host: "test", Home: home, LiveChecks: []LiveCheck{{Repo: "owner/project", Path: home, Command: []string{"approved"}, TimeoutSeconds: 10}}}, HTTP: server.Client(), token: "idh_" + strings.Repeat("a", 43), Now: time.Now, Run: func(_ context.Context, name string, args []string, _ process.Options) (process.Result, error) {
				if name == "git" {
					return process.Result{Started: true, Out: commit + "\n"}, nil
				}
				runs++
				if tc.run == nil {
					t.Fatal("一致しない依頼の確認を実行しました")
				}
				return tc.run()
			}}
			if err := r.runLiveChecks(context.Background()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if runs != tc.runs || len(reasons) != 1 || reasons[0] != tc.reason {
				t.Fatalf("runs=%d reasons=%v", runs, reasons)
			}
		})
	}
}

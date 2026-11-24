package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/intake"
)

func commandFor(t *testing.T, handler http.HandlerFunc) (Command, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	file := filepath.Join(dir, "config.local.json")
	tokenFile := filepath.Join(dir, "token")
	body, _ := json.Marshal(config.Bootstrap{ServerURL: server.URL, ServerTokenFile: "token", WorkerID: "test"})
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("idk_"+strings.Repeat("a", 43)), 0600); err != nil {
		t.Fatal(err)
	}
	out, stderr := new(bytes.Buffer), new(bytes.Buffer)
	return Command{Out: out, Err: stderr, NewClient: func(b config.Bootstrap, token string) (*intake.Client, error) {
		return intake.New(b, token, server.Client().Transport)
	}}, file, out, stderr
}
func envelope(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err, out.String())
	}
	return v
}
func TestGetKeepsHTTPRefusalSeparateFromConnectionFailure(t *testing.T) {
	var calls atomic.Int32
	c, file, out, stderr := commandFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/issue" || r.URL.Query().Get("number") != "7" {
			t.Error("明示した要求と異なります")
		}
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"error":"操作は許可されません","code":"REQUEST_REJECTED"}`)
	})
	code := c.Run(context.Background(), []string{"get", "--config", file, "--path", "/api/issue?number=7"})
	if code != 1 || calls.Load() != 1 || stderr.Len() != 0 {
		t.Fatal(code, calls.Load(), stderr.String())
	}
	v := envelope(t, out)
	if v["status"] != json.Number("403") || v["bodyType"] != "json" || v["body"].(map[string]any)["code"] != "REQUEST_REJECTED" {
		t.Fatal(v)
	}
}
func TestOperationReadsStdinAndKeepsSendIDWithoutRetry(t *testing.T) {
	original := "{\"id\":\"same-id\",\"text\":\"依頼本文\"}\n"
	var calls atomic.Int32
	c, file, out, _ := commandFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		got, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || string(got) != original {
			t.Error("指定のmethod/body/送信IDと異なります")
		}
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"receiptId":"same-id","code":"TEMPORARY_UNAVAILABLE"}`)
	})
	c.In = strings.NewReader(original)
	if code := c.Run(context.Background(), []string{"operation", "--config", file, "--path", "/api/issues", "--method", "POST", "--body-file", "-"}); code != 1 || calls.Load() != 1 {
		t.Fatal(code, calls.Load())
	}
	if envelope(t, out)["body"].(map[string]any)["receiptId"] != "same-id" {
		t.Fatal("応答の受付IDが消えました")
	}
}
func TestOperationReadsFileAndHandles204(t *testing.T) {
	c, file, out, _ := commandFor(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if r.Method != "PUT" || string(got) != `{"epoch":7}` {
			t.Error("本文ファイルが保存されません")
		}
		w.WriteHeader(204)
	})
	bodyFile := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(bodyFile, []byte(`{"epoch":7}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code := c.Run(context.Background(), []string{"operation", "--config", file, "--path", "/api/v1/example", "--method", "PUT", "--body-file", bodyFile}); code != 0 {
		t.Fatal(code)
	}
	v := envelope(t, out)
	if v["status"] != json.Number("204") || v["body"] != nil || v["bodyType"] != "empty" {
		t.Fatal(v)
	}
}
func TestDoctorOnlyReadsConfiguration(t *testing.T) {
	var calls atomic.Int32
	c, file, out, _ := commandFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/api/v1/client-config" || r.URL.Query().Get("host") != "test" {
			t.Error("doctorが設定以外を操作しました")
		}
		_, _ = io.WriteString(w, `{"version":"v","configurationVersion":"c","settingsVersion":"s","config":{"assignments":{}}}`)
	})
	if code := c.Run(context.Background(), []string{"doctor", "--config", file}); code != 0 || calls.Load() != 1 {
		t.Fatal(code, calls.Load())
	}
	if envelope(t, out)["configVersion"] != "v" {
		t.Fatal(out.String())
	}
}
func TestResponseSecretsAreRedactedWithoutLosingNumbers(t *testing.T) {
	token := "idk_" + strings.Repeat("a", 43)
	c, file, out, stderr := commandFor(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"id":9007199254740991,"secret":"hidden-password","csrf":"hidden-csrf","tokenFile":"private/token","nested":["`+token+`",{"access_token":"hidden-access"}],"`+token+`":"key-secret"}`)
	})
	if code := c.Run(context.Background(), []string{"get", "--config", file, "--path", "/api/test"}); code != 0 {
		t.Fatal(code)
	}
	for _, secret := range []string{token, "hidden-password", "hidden-csrf", "hidden-access"} {
		if strings.Contains(out.String()+stderr.String(), secret) {
			t.Fatal("応答から秘密が漏れました")
		}
	}
	v := envelope(t, out)
	body := v["body"].(map[string]any)
	if v["redacted"] != true || body["id"] != json.Number("9007199254740991") || body["tokenFile"] != "private/token" {
		t.Fatal(v)
	}
}
func TestNonJSONBodyAndTransportFailureHaveDifferentOutput(t *testing.T) {
	c, file, out, stderr := commandFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = io.WriteString(w, "gateway: Bearer secret-value")
	})
	if code := c.Run(context.Background(), []string{"get", "--config", file, "--path", "/api/test"}); code != 1 {
		t.Fatal(code)
	}
	v := envelope(t, out)
	if v["status"] != json.Number("502") || v["bodyType"] != "text" || v["redacted"] != true || strings.Contains(out.String(), "secret-value") {
		t.Fatal(v)
	}
	out.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := c.Run(ctx, []string{"get", "--config", file, "--path", "/api/test"}); code != 2 || out.Len() != 0 || stderr.Len() == 0 {
		t.Fatal(code, out.String(), stderr.String())
	}
}
func TestInvalidExplicitArgumentsNeverContactServer(t *testing.T) {
	var calls atomic.Int32
	c, file, out, _ := commandFor(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	cases := [][]string{
		{"get", "--config", file, "--path", "https://outside.example/api/test"},
		{"get", "--config", file, "--path", "/api/test", "--cookie", "secret"},
		{"get", "--config", file, "--path", "/api/test", "--config", file},
		{"operation", "--config", file, "--path", "/api/test", "--method", "GET"},
		{"operation", "--config", file, "--path", "/api/test", "--method", "POST", "--body-file", "-"},
	}
	c.In = strings.NewReader("invalid json")
	for _, args := range cases {
		if code := c.Run(context.Background(), args); code != 2 {
			t.Fatal(code, args)
		}
	}
	if calls.Load() != 0 || out.Len() != 0 {
		t.Fatal("明示されていない要求を送信しました")
	}
}

// doctorが受付に断られたときは、通信の失敗（2）と分けて1で終え、statusを示す。
func TestDoctorRefusalExitsOne(t *testing.T) {
	for _, status := range []int{401, 403, 307} { // Explicitは転送を追わない
		c, file, out, _ := commandFor(t, func(w http.ResponseWriter, r *http.Request) {
			if status == 307 {
				w.Header().Set("Location", "/elsewhere")
			}
			w.WriteHeader(status)
		})
		if code := c.Run(context.Background(), []string{"doctor", "--config", file}); code != 1 {
			t.Fatalf("%d: 終了コード %d", status, code)
		}
		if envelope(t, out)["status"] != json.Number(strconv.Itoa(status)) {
			t.Fatalf("%d: statusを示していません", status)
		}
	}
}

// doctorで応答を得られないときと、2xxでも設定が欠けているときは2で終える。
func TestDoctorTransportAndBadSnapshotExitTwo(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"transport": func(w http.ResponseWriter, r *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
		},
		"missing": func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{"version":"v"}`) },
		"broken":  func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `{`) },
	} {
		c, file, _, _ := commandFor(t, handler)
		if code := c.Run(context.Background(), []string{"doctor", "--config", file}); code != 2 {
			t.Errorf("%s: 終了コード %d", name, code)
		}
	}
}

// APIキーを返す応答は、欄の名前（key）でも値の形（sk-…）でも伏せる。
func TestGetHidesClaudeAPIKey(t *testing.T) {
	c, file, out, _ := commandFor(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"key":"plain-abcdefghij","privateKey":"pem-klmnopqrst","note":"sk-`+strings.Repeat("b", 30)+`","keyName":"visible-name","path":"issue-desk-worker"}`)
	})
	if code := c.Run(context.Background(), []string{"get", "--config", file, "--path", "/api/v1/hosts/test/claude-api-key"}); code != 0 {
		t.Fatal(code)
	}
	if strings.Contains(out.String(), "abcdefghij") || strings.Contains(out.String(), "klmnopqrst") || strings.Contains(out.String(), strings.Repeat("b", 30)) || envelope(t, out)["redacted"] != true {
		t.Fatalf("APIキーが出ました: %s", out.String())
	}
	// 秘密でない欄・普通の文字列は伏せない。
	if !strings.Contains(out.String(), "visible-name") || !strings.Contains(out.String(), "issue-desk-worker") {
		t.Fatalf("伏せすぎました: %s", out.String())
	}
}

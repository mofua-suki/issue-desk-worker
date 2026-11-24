package intake

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExplicitPreservesBodyIDAndHTTPStatusWithoutRetry(t *testing.T) {
	original := json.RawMessage("{\n  \"id\": \"fixed-send-id\", \"epoch\": 9007199254740991\n}\n")
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if string(got) != string(original) {
			t.Error("本文または送信IDを書き換えました")
		}
		if r.Header.Get(ContractHeader) != ContractVersion {
			t.Error("契約headerがありません")
		}
		if r.Header.Get("Cookie") != "" || r.Header.Get("x-desk-key") != "" || r.Header.Get("Origin") != "" {
			t.Error("Bearer以外の認証を追加しました")
		}
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"code":"TEMPORARY_UNAVAILABLE","retryable":"same-request"}`)
	})
	response, err := c.Explicit(context.Background(), "POST", "/api/issues", original)
	if err != nil || response.Status != 503 || !strings.Contains(string(response.Body), "TEMPORARY_UNAVAILABLE") {
		t.Fatal(response, err)
	}
	if calls.Load() != 1 {
		t.Fatal("操作を自動再送しました", calls.Load())
	}
}
func TestExplicitRefusesURLsTraversalAndMalformedJSON(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	for _, target := range []string{"https://other.example/api/x", "//other.example/api/x", "/admin", "/api/../admin", "/api/%2e%2e/admin", "/api//x", "/api/x#fragment", "/api/x\\y"} {
		if _, err := c.Explicit(context.Background(), "GET", target, nil); err == nil {
			t.Errorf("不正targetを受理しました: %q", target)
		}
	}
	if _, err := c.Explicit(context.Background(), "POST", "/api/issues", json.RawMessage(`{"id":`)); err == nil {
		t.Fatal("不正JSONを送信しました")
	}
	if _, err := c.Explicit(context.Background(), "GET", "/api/issues", json.RawMessage(`{}`)); err == nil {
		t.Fatal("読取要求に本文を送りました")
	}
	if calls.Load() != 0 {
		t.Fatal("拒否対象をサーバーへ送りました")
	}
}
func TestExplicitRedirectIsAResponseAndIsNotFollowed(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Location", "https://other.example/api/x")
		w.WriteHeader(307)
		_, _ = io.WriteString(w, "redirect")
	})
	response, err := c.Explicit(context.Background(), "GET", "/api/issues", nil)
	if err != nil || response.Status != 307 || string(response.Body) != "redirect" || calls.Load() != 1 {
		t.Fatal(response, err)
	}
}

// 明示要求の通信の失敗も、通常の経路と同じく失敗の種類（通信の失敗）として分かる。
func TestExplicitClassifiesTransportFailure(t *testing.T) {
	var calls int
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	_, err := c.Explicit(context.Background(), "GET", "/api/v1/hosts", nil)
	var transport *TransportError
	if !errors.As(err, &transport) || !strings.Contains(err.Error(), "自動再送していません") {
		t.Fatalf("通信の失敗が分かりません: %v", err)
	}
	if calls != 1 {
		t.Fatalf("明示要求を送り直しました: %d", calls)
	}
}

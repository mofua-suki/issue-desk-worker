package intake

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// 受付の拒否は、どの要求が断られたか（メソッドとパスの形）を示す。
func TestAPIErrorNamesCall(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	err := c.Call(context.Background(), "POST", "/api/v1/jobs/12/state", map[string]any{}, nil)
	var refusal *APIError
	if !errors.As(err, &refusal) || refusal.Call != "POST /api/v1/jobs/{n}/state" || !strings.Contains(err.Error(), refusal.Call) {
		t.Fatalf("断られた要求が分かりません: %v", err)
	}
}

// 分類できない通信の失敗（中継機の壊れた応答など）も、一時的な接続の失敗として扱う。証明書の失敗は一時的にしない。
func TestUnclassifiedTransportFailureIsTemporary(t *testing.T) {
	if got := TemporaryFailureReason(transportFailure(context.Background(), errors.New("malformed HTTP response"), "")); got != "connection_failed" {
		t.Fatalf("一時的な失敗になりません: %q", got)
	}
	if got := TemporaryFailureReason(transportFailure(context.Background(), &tls.CertificateVerificationError{Err: errors.New("x")}, "")); got != "" {
		t.Fatalf("証明書の失敗を一時的にしました: %q", got)
	}
}

// 通信の失敗・転送・上限・不正なJSONも、どの要求かを方法と経路の型で示す。値（番号）は出さない。
func TestLocalFailuresNameTheCall(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"転送":      func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		"上限":      func(w http.ResponseWriter, r *http.Request) { w.Write(bytes.Repeat([]byte("a"), maxResponseBytes+1)) },
		"不正なJSON": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{")) },
	} {
		t.Run(name, func(t *testing.T) {
			c := clientFor(t, handler)
			var out map[string]any
			err := c.Call(context.Background(), "GET", "/api/v1/jobs/12/state", nil, &out)
			if err == nil || !strings.Contains(err.Error(), "GET /api/v1/jobs/{n}/state") || strings.Contains(err.Error(), "12") {
				t.Fatalf("要求が分かりません: %v", err)
			}
		})
	}
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	})
	err := c.Call(context.Background(), "GET", "/api/v1/jobs/12/state", nil, nil)
	var transport *TransportError
	if !errors.As(err, &transport) || transport.Call != "GET /api/v1/jobs/{n}/state" || TemporaryFailureReason(err) != "connection_failed" {
		t.Fatalf("接続の失敗の要求が分かりません: %v", err)
	}
}

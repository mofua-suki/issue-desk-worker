package webhook

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type unreadableBody struct{ t *testing.T }

func (b unreadableBody) Read([]byte) (int, error) {
	b.t.Fatal("本文を読みました")
	return 0, nil
}
func (unreadableBody) Close() error { return nil }
func TestAuthenticatedWebhookOnlyWakesAndNeverReadsPayload(t *testing.T) {
	hash := sha256.Sum256([]byte("Bearer allowed"))
	r, err := New(hex.EncodeToString(hash[:]))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, auth string
		status             int
	}{{"POST", Path, "Bearer bad", 401}, {"GET", Path, "Bearer allowed", 404}, {"POST", Path + "?x=1", "Bearer allowed", 404}, {"POST", Path, "Bearer allowed", 202}} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Body = unreadableBody{t}
		req.Header.Set("Authorization", tc.auth)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != tc.status {
			t.Fatal(w.Code, tc.status)
		}
	}
	select {
	case <-r.Wake():
	default:
		t.Fatal("認証済みの合図がありません")
	}
	if err := r.SetVerifier(""); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", Path, strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer allowed")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatal("登録待ちが許可されました")
	}
}

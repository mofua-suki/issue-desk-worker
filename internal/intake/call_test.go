package intake

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Call は call と同じ1本の経路で送る。契約の版を付け、204は読まず、知らない拒否コードは固定値にし、送信JSONを作れなければ送らない。
func TestCallSharesTheRequestPath(t *testing.T) {
	var calls int
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get(ContractHeader) != ContractVersion {
			t.Error("契約の版を付けていません")
		}
		switch r.URL.Path {
		case "/api/v1/empty":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/refused":
			w.WriteHeader(409)
			_, _ = io.WriteString(w, `{"code":"secret_idk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)
		case "/api/v1/broken":
			_, _ = io.WriteString(w, `{"ok":`)
		case "/api/v1/turn":
			w.WriteHeader(409)
			_, _ = io.WriteString(w, `{"code":"turn_lost"}`)
		}
	})
	ctx := context.Background()
	var out struct{ OK bool }
	if err := c.Call(ctx, "POST", "/api/v1/empty", map[string]any{}, &out); err != nil {
		t.Fatalf("204を不正な応答にしました: %v", err)
	}
	if err := c.Call(ctx, "GET", "/api/v1/broken", nil, &out); err == nil {
		t.Fatal("壊れたJSON応答を成功にしました")
	}
	var refusal *APIError
	if err := c.Call(ctx, "POST", "/api/v1/refused", nil, nil); !errors.As(err, &refusal) || refusal.Status != 409 || refusal.Code != "request_failed" || strings.Contains(err.Error(), "secret") {
		t.Fatalf("知らない拒否コード: %v", err)
	}
	if err := c.Call(ctx, "POST", "/api/v1/turn", nil, nil); !errors.As(err, &refusal) || refusal.Code != "turn_lost" {
		t.Fatalf("既知の拒否コード: %v", err)
	}
	before := calls
	if err := c.Call(ctx, "POST", "/api/v1/empty", map[string]any{"bad": func() {}}, nil); err == nil || calls != before {
		t.Fatalf("作れないJSONを送りました: %v", err)
	}
	if err := c.Call(ctx, "GET", "https://example.invalid/api/v1/empty", nil, nil); err == nil || calls != before {
		t.Fatalf("APIパス以外へ送りました: %v", err)
	}
}

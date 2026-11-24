package gitea

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"
)

// 拒否応答と通信の失敗は、呼んだメソッドとパスの形と、通信の失敗の種類を示す。
func TestRequestErrorsNameCallAndKind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cut") {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(422)
	}))
	defer server.Close()
	c, err := New(server.URL, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("refusal", func(t *testing.T) {
		err := c.Request(context.Background(), "POST", "/repos/owner/repo/pulls/7/reviews", map[string]string{}, nil)
		var refusal *Error
		if !errors.As(err, &refusal) || refusal.Status != 422 || refusal.Call != "POST /repos/owner/repo/pulls/{n}/reviews" || !strings.Contains(err.Error(), refusal.Call) {
			t.Fatalf("拒否の呼び出しが分かりません: %v", err)
		}
	})
	t.Run("cut", func(t *testing.T) {
		err := c.Request(context.Background(), "GET", "/repos/owner/repo/pulls/3/cut", nil, nil)
		var unreachable *UnreachableError
		if !errors.Is(err, ErrUnreachable) || !errors.As(err, &unreachable) || unreachable.Kind != "接続が途中で切れました" || unreachable.Call != "GET /repos/owner/repo/pulls/{n}/cut" {
			t.Fatalf("途中で切れたことが分かりません: %v", err)
		}
	})
	t.Run("refused", func(t *testing.T) {
		refused, err := New("http://refused.invalid", "fixture")
		if err != nil {
			t.Fatal(err)
		}
		refused.http.Transport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
		}}
		err = refused.Request(context.Background(), "GET", "/repos/owner/repo", nil, nil)
		var unreachable *UnreachableError
		if !errors.As(err, &unreachable) || unreachable.Kind != "接続を拒否されました" || strings.Contains(err.Error(), "refused.invalid") {
			t.Fatalf("接続の拒否が分かりません: %v", err)
		}
	})
}

func TestUnreachableKind(t *testing.T) {
	for want, err := range map[string]error{
		"時間切れ":          fmt.Errorf("wrap: %w", context.DeadlineExceeded),
		"名前を解決できません":    &net.DNSError{Err: "no such host", Name: "x.invalid"},
		"TLSの確認に失敗しました": &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
		"接続を拒否されました":    syscall.ECONNREFUSED,
		"接続が途中で切れました":   syscall.ECONNRESET,
		"通信に失敗しました":     errors.New("other"),
	} {
		if got := unreachableKind(err); got != want {
			t.Errorf("%v: %s（期待: %s）", err, got, want)
		}
	}
}

package cause

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestNewKeepsKindWithoutValues(t *testing.T) {
	_, openErr := os.Open("/private/secret-path/none")
	var decoded struct{ N int }
	typeErr := json.Unmarshal([]byte(`{"N":"x"}`), &decoded)
	syntaxErr := json.Unmarshal([]byte(`{broken`), &decoded)
	for want, err := range map[string]error{
		"設定を読めません（見つかりません）":                                 openErr,
		"設定を読めません（権限がありません）":                                &fs.PathError{Op: "open", Path: "/private/x", Err: fs.ErrPermission},
		"設定を読めません（時間切れ）":                                    fmt.Errorf("x: %w", context.DeadlineExceeded),
		"設定を読めません（JSONの型が違います）":                             typeErr,
		"設定を読めません（中身がありません）":                                io.EOF,
		"設定を読めません（途中で終わりました）":                               io.ErrUnexpectedEOF,
		"設定を読めません（見つかりません）（exec）":                           &exec.Error{Name: "tool", Err: exec.ErrNotFound},
		"設定を読めません（接続を拒否されました）":                              &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)},
		"設定を読めません（名前を解決できません）":                              &net.DNSError{Err: "no such host", Name: "x.invalid"},
		"設定を読めません（時間切れ）（入れ子）":                               New("内側", context.DeadlineExceeded),
		"設定を読めません（JSONの構文が不正です（2バイト目））":                     syntaxErr,
		"設定を読めません（システムの失敗（" + syscall.ENOSPC.Error() + "））": syscall.ENOSPC,
		"設定を読めません（*errors.errorString）":                     errors.New("token=secret-value"),
	} {
		got := New("設定を読めません", err).Error()
		want = strings.TrimSuffix(strings.TrimSuffix(want, "（exec）"), "（入れ子）")
		if got != want {
			t.Errorf("%v: %s（期待: %s）", err, got, want)
		}
		if strings.Contains(got, "private") || strings.Contains(got, "secret") {
			t.Errorf("値が出ました: %s", got)
		}
	}
	if got := New("設定を読めません", nil).Error(); got != "設定を読めません" {
		t.Fatal(got)
	}
	// 元の失敗では判定させない（従来の固定のエラーと同じ扱いにする）。
	if errors.Is(New("x", context.Canceled), context.Canceled) {
		t.Fatal("元の失敗を辿れます")
	}
}

type selfDescribed struct{}

func (selfDescribed) Error() string     { return "secret-body" }
func (selfDescribed) CauseKind() string { return "HTTP 503" }

// 種類を自分で名乗るエラーは、その種類を使う。
func TestKindUsesCauseKind(t *testing.T) {
	if got := New("x", fmt.Errorf("w: %w", selfDescribed{})).Error(); got != "x（HTTP 503）" {
		t.Fatal(got)
	}
}

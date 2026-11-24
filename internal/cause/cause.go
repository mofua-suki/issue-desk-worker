// Package cause keeps why an operation failed in an error whose message is
// otherwise a fixed sentence. Only the kind of the underlying failure is
// shown, never its text, so paths, URLs, secrets and remote bodies stay out
// of messages that are logged or sent to the intake. The underlying error is
// deliberately not unwrapped: callers keep deciding on the fixed error as
// before, and only the message gains the reason.
package cause

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// Error is a fixed message with the kind of its underlying failure.
type Error struct {
	Message string
	Kind    string
}

func (e *Error) Error() string {
	if e.Kind == "" {
		return e.Message
	}
	return e.Message + "（" + e.Kind + "）"
}

// New returns message as an error, with the kind of err when err is not nil.
func New(message string, err error) error {
	if err == nil {
		return errors.New(message)
	}
	return &Error{Message: message, Kind: Kind(err)}
}

// Kind names the kind of err without any of its values. An error type can
// name its own kind with a CauseKind method, which must not carry values.
func Kind(err error) string {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	var urlErr *url.Error
	var dns *net.DNSError
	var errno syscall.Errno
	var kinded *Error
	var described interface{ CauseKind() string }
	switch {
	case err == nil:
		return ""
	case errors.As(err, &kinded):
		return kinded.Kind
	case errors.As(err, &described):
		return described.CauseKind()
	case errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err):
		return "時間切れ"
	case errors.Is(err, context.Canceled):
		return "取り消されました"
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, exec.ErrNotFound):
		return "見つかりません"
	case errors.Is(err, fs.ErrPermission):
		return "権限がありません"
	case errors.Is(err, fs.ErrExist):
		return "既にあります"
	case errors.As(err, &syntax):
		return "JSONの構文が不正です（" + strconv.FormatInt(syntax.Offset, 10) + "バイト目）"
	case errors.As(err, &typeErr):
		// Field can carry keys from the input, so it is not shown.
		return "JSONの型が違います"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "途中で終わりました"
	case errors.Is(err, io.EOF):
		return "中身がありません"
	case errors.As(err, &dns):
		return "名前を解決できません"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "接続を拒否されました"
	case errors.Is(err, syscall.ECONNRESET):
		return "接続が切れました"
	case errors.As(err, &errno):
		return "システムの失敗（" + errno.Error() + "）"
	case errors.As(err, &urlErr):
		return "URLの扱いに失敗しました（" + urlErr.Op + "）"
	}
	return fmt.Sprintf("%T", err)
}

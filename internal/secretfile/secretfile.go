// Package secretfile reads small secrets from files that only the owner can access.
package secretfile

import (
	"errors"
	"io"
	"issue-desk-worker/internal/cause"
	"os"
)

// Failures that callers translate into their own messages.
var (
	ErrOpen        = errors.New("secret file cannot be opened")
	ErrUnprotected = errors.New("secret file is not an owner-only regular file")
	ErrRead        = errors.New("secret file cannot be read within the size limit")
)

// Read returns the contents of file when it is a regular file without group or
// other permissions and holds at most limit bytes.
func Read(file string, limit int64) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, &failure{ErrOpen, cause.Kind(err)}
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, &failure{ErrUnprotected, cause.Kind(err)}
	}
	if !info.Mode().IsRegular() {
		return nil, &failure{ErrUnprotected, "通常のファイルではありません"}
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, &failure{ErrUnprotected, "所有者以外も読めます"}
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, &failure{ErrRead, cause.Kind(err)}
	}
	if int64(len(data)) > limit {
		return nil, &failure{ErrRead, "上限より大きいです"}
	}
	return data, nil
}

// failure is one of the sentinel errors with the kind of what went wrong,
// never the file's path or contents.
type failure struct {
	sentinel error
	kind     string
}

func (f *failure) Error() string { return f.sentinel.Error() + "（" + f.kind + "）" }
func (f *failure) Unwrap() error { return f.sentinel }

// Message is the error a caller shows for err, naming the secret as what.
func Message(what string, err error) error {
	kind := ""
	var f *failure
	if errors.As(err, &f) {
		kind = "（" + f.kind + "）"
	}
	switch {
	case errors.Is(err, ErrOpen):
		return errors.New(what + "を開けません" + kind)
	case errors.Is(err, ErrUnprotected):
		return errors.New(what + "は所有者だけが読める通常ファイルにしてください" + kind)
	default:
		return errors.New(what + "を読めません" + kind)
	}
}

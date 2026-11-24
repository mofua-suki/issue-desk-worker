package apple

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

type nativeTransport func(*http.Request) (*http.Response, error)

func (f nativeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func nativeResponse(status int, v any) *http.Response {
	b, _ := json.Marshal(v)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b)))}
}
func shellArg(v string) string { return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'" }

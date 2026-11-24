package gitea

import (
	"strings"
	"testing"
)

func TestPlainHTTPWarning(t *testing.T) {
	if w := PlainHTTPWarning("http://192.168.0.10:3000"); !strings.Contains(w, "192.168.0.10:3000") || !strings.Contains(w, "http") {
		t.Fatalf("httpの警告がありません: %q", w)
	}
	for _, base := range []string{"https://gitea.example", "HTTPS://gitea.example", "::bad"} {
		if w := PlainHTTPWarning(base); w != "" {
			t.Fatalf("%sで警告しました: %q", base, w)
		}
	}
}

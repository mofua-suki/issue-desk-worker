package failurelog

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 同じ失敗は間隔を空けて回数とともに記録し、違う失敗や成功の後の失敗はすぐ記録する。交互に来ても失敗ごとに間引く。
func TestLogThinsRepeats(t *testing.T) {
	var out strings.Builder
	now := time.Unix(0, 0)
	log := &Log{Out: &out, Label: "P", Thinner: Thinner{Now: func() time.Time { return now }}}
	for i := 0; i < 3; i++ {
		log.Observe(errors.New("a"))
		log.Observe(errors.New("b"))
		now = now.Add(time.Minute)
	}
	now = now.Add(Repeat)
	log.Observe(errors.New("a"))
	log.Observe(nil)
	log.Observe(errors.New("b"))
	want := "P: a\nP: b\nP: a（前回の記録から同じ失敗が3回）\nP: b\n"
	if out.String() != want {
		t.Fatalf("%q", out.String())
	}
}

func TestLogRedactsBeforeComparing(t *testing.T) {
	var out strings.Builder
	log := &Log{Out: &out, Label: "P", Redact: func(s string) string { return strings.ReplaceAll(s, "secret", "[REDACTED]") }}
	log.Observe(errors.New("token secret"))
	if out.String() != "P: token [REDACTED]\n" {
		t.Fatalf("%q", out.String())
	}
}

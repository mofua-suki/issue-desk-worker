// Package failurelog thins repeated failures of periodic work: a failure is
// reported when it first occurs, and the same failure again at most every
// Repeat with how many times it occurred since. A success clears what was
// seen, so the next failure is reported at once.
package failurelog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Repeat is how often the same failure is reported again while it continues.
const Repeat = 10 * time.Minute

// Thinner decides which failures to report. Failures are remembered by
// message, so two failures that alternate are both thinned. It is safe for
// concurrent use.
type Thinner struct {
	// Now is the clock; time.Now when nil.
	Now  func() time.Time
	mu   sync.Mutex
	seen map[string]*seen
}

type seen struct {
	at      time.Time
	skipped int
}

// Pass reports whether err should be reported now, with err annotated by the
// number of occurrences since its last report when some were thinned. A nil
// err is a success.
func (t *Thinner) Pass(err error) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		t.seen = nil
		return false, nil
	}
	now := time.Now()
	if t.Now != nil {
		now = t.Now()
	}
	if t.seen == nil {
		t.seen = map[string]*seen{}
	}
	message := err.Error()
	last := t.seen[message]
	if last != nil && now.Sub(last.at) < Repeat {
		last.skipped++
		return false, nil
	}
	if last != nil && last.skipped > 0 {
		err = fmt.Errorf("%w（前回の記録から同じ失敗が%d回）", err, last.skipped+1)
	}
	// Forget failures that stopped: at once when nothing was thinned, and
	// after a day when only their count would be lost.
	for other, entry := range t.seen {
		if age := now.Sub(entry.at); age >= Repeat && entry.skipped == 0 || age >= 24*time.Hour {
			delete(t.seen, other)
		}
	}
	t.seen[message] = &seen{at: now}
	return true, err
}

// Log writes the failures a Thinner passes as "Label: message" lines.
type Log struct {
	Thinner
	// Out is where lines go; os.Stderr when nil.
	Out io.Writer
	// Label names the failed work.
	Label string
	// Redact, when set, hides secrets in the message before it is compared
	// and written.
	Redact func(string) string
}

// Observe records the outcome of one round; a nil err is a success.
func (l *Log) Observe(err error) {
	if err != nil && l.Redact != nil {
		err = errors.New(l.Redact(err.Error()))
	}
	ok, reported := l.Pass(err)
	if !ok {
		return
	}
	out := l.Out
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintf(out, "%s: %s\n", l.Label, reported)
}

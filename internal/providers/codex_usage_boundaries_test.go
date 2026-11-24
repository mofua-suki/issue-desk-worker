package providers

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func appendUsageRecord(t *testing.T, name, record string) {
	t.Helper()
	f, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(record)
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal(writeErr, closeErr)
	}
}

func TestCodexRecoveryAcrossLargeBaselineAndExecution(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	// The retained baseline tail begins inside a valid large record, while the
	// last cumulative sample precedes that tail. Full-baseline scanning finds it.
	pad := `{"note":"` + strings.Repeat("x", limitTailBytes/2) + `"}`
	writeRollout(t, home, "abc", now, usageEvent(100, 20, 30), pad, pad, pad)
	b := snapshotCodexUsage(home, "abc", now)
	if !b.valid || b.offset <= 0 {
		t.Fatalf("large baseline was not usable: %+v", b)
	}
	appendUsageRecord(t, b.path, pad+"\n"+pad+"\n"+pad+"\n"+usageEvent(120, 25, 33)+"\n")
	u := recoverCodexUsage(home, "abc", now, true, b)
	want := codexUsagePoint{20, 5, 3}
	checkCodexTokens(t, u, &want)
}

func TestCodexRecoveryRejectsOversizedNewSession(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	meta := fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"new"}}`, now.Add(time.Second).UTC().Format(time.RFC3339Nano))
	pad := `{"note":"` + strings.Repeat("x", limitTailBytes/2) + `"}`
	writeRollout(t, home, "new", now, meta, pad, pad, pad, usageEvent(120, 25, 33))
	if u := recoverCodexUsage(home, "new", now, false, codexUsageBaseline{}); u.Input != nil || u.Cached != nil || u.Output != nil {
		t.Fatalf("oversized new session accepted: %+v", u)
	}
}

func TestCodexRecoveryRejectsIncompleteFinalRecord(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	writeRollout(t, home, "abc", now, usageEvent(100, 20, 30))
	b := snapshotCodexUsage(home, "abc", now)
	appendUsageRecord(t, b.path, usageEvent(120, 25, 33)+"\n"+`{"type":"event_msg"`)
	if u := recoverCodexUsage(home, "abc", now, true, b); u.Input != nil || u.Cached != nil || u.Output != nil {
		t.Fatalf("incomplete range accepted: %+v", u)
	}
}

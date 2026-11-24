package execution

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"issue-desk-worker/internal/atomicfile"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// Lease diagnostics are separate from job journals, so recording a loss cannot
// overwrite a saved result, usage, or a newer in-memory execution record.
type leaseDiagnostic struct {
	Job        int64             `json:"job"`
	Generation *int              `json:"generation"`
	Epoch      int64             `json:"epoch"`
	At         time.Time         `json:"at"`
	Reason     string            `json:"reason"`
	Stage      string            `json:"stage"`
	Millis     map[string]*int64 `json:"duration_ms"`
	Gaps       bool              `json:"incomplete"`
}

var diagnosticStages = []string{"implementing", "test", "review", "publish", "merge", "deploy"}

func diagnosticDir(s *journal.Store) string {
	return filepath.Join(s.Root, "state", "lease-diagnostics")
}

func releasePersistenceFailure(id, epoch int64) error {
	log.Printf("lease job=%d epoch=%d event=release_result_save_failed", id, epoch)
	return fmt.Errorf("%w: 解放記録を保存できません", intake.ErrJobReleased)
}

func (e *Executor) recordScopeRelease(r *journal.Record) error {
	raw, _ := json.Marshal(struct {
		Generation int `json:"generation"`
	}{r.Generation})
	startedAt := time.Now().UTC()
	if r.Timing != nil {
		startedAt = r.Timing.StartedAt
	}
	err := e.RecordLeaseDiagnostic(intake.Job{ID: r.ID, LeaseEpoch: r.Epoch, Raw: raw}, "scope_wait", time.Now(), startedAt)
	if err != nil {
		log.Printf("lease job=%d epoch=%d event=diagnostic_save_failed", r.ID, r.Epoch)
	}
	return err
}

func (e *Executor) RecordLeaseDiagnostic(job intake.Job, reason string, at, startedAt time.Time) error {
	switch reason {
	case "lease_expired", "lease_lost", "response_mismatch", "renew_rejected", "scope_wait":
	default:
		return errors.New("lease診断の理由が不正です")
	}
	var descriptor struct {
		Generation *int `json:"generation"`
	}
	_ = json.Unmarshal(job.Raw, &descriptor)
	d := leaseDiagnostic{Job: job.ID, Epoch: job.LeaseEpoch, Generation: descriptor.Generation, At: at.UTC(), Reason: reason, Stage: "unknown", Millis: map[string]*int64{}, Gaps: true}
	for _, stage := range diagnosticStages {
		d.Millis[stage] = nil
	}
	r, loadErr := e.Store.Load(job.ID)
	if r != nil && r.Timing != nil && !r.Timing.StartedAt.Before(startedAt) && !at.Before(r.Timing.StartedAt) && r.Timing.Epoch == job.LeaseEpoch && descriptor.Generation != nil && r.Timing.Generation == *descriptor.Generation {
		timing := r.Timing.SnapshotAt(at)
		d.Gaps = timing.Gaps
		switch timing.LastStage {
		case "implement", "implement_report", "test", "test_report", "review", "publish", "merge", "deploy":
			d.Stage = timing.LastStage
		}
		for k, value := range timing.Millis {
			if _, ok := d.Millis[k]; ok {
				v := value
				d.Millis[k] = &v
			}
		}
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	log.Printf("lease_diagnostic %s", data)
	dir := diagnosticDir(e.Store)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return cause.New("lease診断の保存先が不正です", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	name := fmt.Sprintf("job-%d-%s.json", job.ID, hex.EncodeToString(nonce[:]))
	return errors.Join(loadErr, atomicfile.Write(filepath.Join(dir, name), ".lease-*.tmp", 0600, true, data, nil))
}

// Keep every diagnostic locally. Existing final states are never rewritten.
func (e *Executor) leaseDiagnosticSummary(id int64, summary string) (string, error) {
	if e.Store == nil {
		return summary, nil
	}
	dir := diagnosticDir(e.Store)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return summary, nil
	}
	if err != nil {
		return summary, err
	}
	var records []leaseDiagnostic
	var readErr error
	prefix := fmt.Sprintf("job-%d-", id)
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if !entry.Type().IsRegular() {
			readErr = errors.New("lease診断ファイルが通常ファイルではありません")
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			readErr = err
			continue
		}
		var d leaseDiagnostic
		if json.Unmarshal(data, &d) != nil || d.Job != id {
			readErr = errors.New("lease診断ファイルが不正です")
			continue
		}
		records = append(records, d)
	}
	if len(records) == 0 {
		return summary, readErr
	}
	sort.Slice(records, func(i, j int) bool { return records[i].At.Before(records[j].At) })
	parts := make([]string, 0, len(records))
	for i := len(records) - 1; i >= 0; i-- {
		data, err := json.Marshal(records[i])
		if err != nil {
			readErr = err
			continue
		}
		candidate := append(parts, string(data))
		suffix := fmt.Sprintf("\nlease診断(新しい順%d/%d件、state/lease-diagnosticsに保持): %s", len(candidate), len(records), strings.Join(candidate, " "))
		if len([]rune(summary))+len([]rune(suffix)) > 2000 {
			break
		}
		parts = candidate
	}
	if len(parts) == 0 {
		return summary, readErr
	}
	return fmt.Sprintf("%s\nlease診断(新しい順%d/%d件、state/lease-diagnosticsに保持): %s", summary, len(parts), len(records), strings.Join(parts, " ")), readErr
}

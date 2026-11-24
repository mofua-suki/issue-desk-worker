package execution

import (
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/journal"
	"os"
	"strings"
	"time"
)

var antigravityReleasePath = "/app/deploy/antigravity-release.json"

func antigravityHoldVersion() string {
	var release struct {
		Version string `json:"version"`
	}
	raw, err := os.ReadFile(antigravityReleasePath)
	if err != nil || len(raw) > 65536 || json.Unmarshal(raw, &release) != nil || strings.TrimSpace(release.Version) == "" {
		return "不明#r2"
	}
	return strings.TrimSpace(release.Version) + "#r2"
}
func validAGYHold(h *journal.ProviderDenialHold, version string) bool {
	if h == nil || h.Version != version || h.Count <= 0 || h.Inside != h.Count || h.Pathless < 0 || h.Job == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, h.At)
	return err == nil
}
func (e *Executor) antigravityHold(r *journal.Record) (*journal.ProviderDenialHold, error) {
	version := antigravityHoldVersion()
	if validAGYHold(r.AntigravityDenialHold, version) {
		return r.AntigravityDenialHold, nil
	}
	records, err := e.Store.List()
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		if validAGYHold(record.AntigravityDenialHold, version) {
			return record.AntigravityDenialHold, nil
		}
	}
	return nil, nil
}
func (e *Executor) recordAntigravityDenied(d Descriptor, r *journal.Record, denied []json.RawMessage) (denialKinds, error) {
	kinds := e.classifyAGYDenials(denied)
	if len(denied) == 0 {
		return kinds, nil
	}
	at := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	actions := []string{}
	for i, raw := range denied {
		if i >= 20 {
			break
		}
		text := string(raw)
		var plain string
		if json.Unmarshal(raw, &plain) == nil {
			text = plain
		}
		runes := []rune(e.redact(text))
		if len(runes) > 300 {
			runes = runes[:300]
		}
		actions = append(actions, string(runes))
	}
	r.AntigravityDenied = append(r.AntigravityDenied, journal.ProviderDenied{Generation: d.Generation, At: at, Count: len(denied), Actions: actions, Unexpected: kinds.Inside + kinds.Pathless, Inside: kinds.Inside, Pathless: kinds.Pathless})
	if len(r.AntigravityDenied) > 50 {
		r.AntigravityDenied = r.AntigravityDenied[len(r.AntigravityDenied)-50:]
	}
	if kinds.Inside > 0 {
		version := antigravityHoldVersion()
		if !validAGYHold(r.AntigravityDenialHold, version) {
			r.AntigravityDenialHold = &journal.ProviderDenialHold{Version: version, At: at, Job: fmt.Sprintf("%s#%d", r.Repo, r.Issue), Count: kinds.Inside, Inside: kinds.Inside, Pathless: kinds.Pathless}
		}
	}
	if err := e.Store.Save(r); err != nil {
		return kinds, err
	}
	return kinds, nil
}

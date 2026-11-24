package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
	"os"
	"strings"
)

const limitBatchRows = 500

// limitRows renders readings with exactly the intake's limit fields. Rows the intake
// would reject (no host or window name) are left out, so one unreadable value cannot
// cost a usage report.
func limitRows(host string, readings []providers.LimitReading) []json.RawMessage {
	if strings.TrimSpace(host) == "" {
		return nil
	}
	rows := []json.RawMessage{}
	for _, reading := range readings {
		if strings.TrimSpace(reading.Limit) == "" {
			continue
		}
		rows = append(rows, encode(map[string]any{"at": reading.At.UTC().Format("2006-01-02T15:04:05.000Z"), "host": host, "source": "worker", "provider": reading.Provider, "limit": reading.Limit, "status": reading.Status, "utilization": reading.Utilization, "percent": reading.Percent, "resetsAt": reading.ResetsAt, "windowMinutes": reading.WindowMinutes}))
	}
	return rows
}

// flushLimits reports held rows on their own request, separate from usage, because
// usage failures stop a run and limit failures must not. Undelivered rows stay in the
// journal for the next flush; a repeat after a crash is deduplicated by the intake's
// content key. Rows the intake refuses are dropped so they cannot retry forever.
func (e *Executor) flushLimits(ctx context.Context, r *journal.Record) error {
	for len(r.Limits) > 0 {
		end := min(limitBatchRows, len(r.Limits))
		response, err := e.API.Explicit(ctx, "POST", "/api/v1/usage", encode(map[string]any{"usage": []any{}, "limits": r.Limits[:end]}))
		if err != nil {
			fmt.Fprintln(os.Stderr, "利用枠の使用率を送れませんでした。次の報告で送り直します:", err)
			return nil
		}
		if response.Status < 200 || response.Status >= 300 {
			fmt.Fprintln(os.Stderr, "利用枠の使用率を受付が保存しませんでした。次の報告で送り直します: HTTP", response.Status)
			return nil
		}
		rejected, valid := usageReceipt(response.Body, end)
		if !valid {
			fmt.Fprintln(os.Stderr, "利用枠の使用率の応答が不正です。次の報告で送り直します")
			return nil
		}
		if rejected > 0 {
			fmt.Fprintln(os.Stderr, "利用枠の使用率のうち受付が拒否した行を捨てました:", rejected)
		}
		r.Limits = append([]json.RawMessage(nil), r.Limits[end:]...)
		if err := e.Store.Save(r); err != nil {
			return err
		}
	}
	return nil
}

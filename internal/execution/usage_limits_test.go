package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"issue-desk-worker/internal/jobspec"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

// Fields and formats the intake's ValidLimit accepts (server/internal/usage/validation.go).
var intakeLimitFields = map[string]bool{"at": true, "host": true, "source": true, "provider": true, "limit": true, "status": true, "utilization": true, "percent": true, "resetsAt": true, "windowMinutes": true}

func checkIntakeLimit(t *testing.T, row map[string]any) {
	t.Helper()
	for key := range row {
		if !intakeLimitFields[key] {
			t.Fatalf("受付が拒否する項目です: %s", key)
		}
	}
	for _, key := range []string{"at", "resetsAt"} {
		if value, ok := row[key].(string); ok {
			at, err := time.Parse(time.RFC3339Nano, value)
			if err != nil || at.UTC().Format("2006-01-02T15:04:05.000Z") != value {
				t.Fatalf("受付が拒否する日時です: %s=%s", key, value)
			}
		} else if key == "at" || row[key] != nil {
			t.Fatalf("%s: %v", key, row[key])
		}
	}
	if row["source"] != "worker" || row["host"] != "gt12" || (row["provider"] != "claude" && row["provider"] != "codex") || row["limit"] == "" {
		t.Fatalf("%v", row)
	}
}

type limitServer struct {
	bodies   []map[string][]map[string]any
	status   int
	rejected int
	response string
}

func (s *limitServer) client(t *testing.T) *intake.Client {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/usage" {
			t.Errorf("予期しない経路: %s", r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string][]map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Error(err)
		}
		s.bodies = append(s.bodies, body)
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		if s.response != "" {
			_, _ = io.WriteString(w, s.response)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"added": len(body["usage"]) + len(body["limits"]) - s.rejected, "duplicate": 0, "rejected": s.rejected})
	})
	transport := nativeTransport(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Result(), nil
	})
	client, err := intake.New(configBootstrap("https://fixture.invalid"), "idk_"+strings.Repeat("a", 43), transport)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func limitExecutor(t *testing.T, s *limitServer) (*Executor, *journal.Record) {
	t.Helper()
	store, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &Executor{Store: store, API: s.client(t)}
	e.Config.Host = "gt12"
	return e, &journal.Record{ID: 1, Epoch: 1}
}

func sentLimits(s *limitServer) []map[string]any {
	rows := []map[string]any{}
	for _, body := range s.bodies {
		rows = append(rows, body["limits"]...)
	}
	return rows
}

// A provider's real output must reach the request body through the journal, so that a
// return to sending empty limits fails here.
func TestUsageLimitsTravelFromOutputThroughJournalToRequest(t *testing.T) {
	s := &limitServer{}
	e, r := limitExecutor(t, s)
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1794024000,"rateLimitType":"seven_day","overageStatus":"rejected","isUsingOverage":false,"unifiedWindows":{"seven_day":{"utilization":0.29,"resetsAt":1794024000,"status":"allowed"}}}}`
	answer := providers.Answer{Outcome: "completed", Limits: providers.ClaudeLimits(line, now)}
	if err := e.recordUsage(r, Descriptor{Descriptor: jobspec.Descriptor{Repo: "o/r", Issue: 1}}, providers.Settings{Provider: "claude"}, "独立レビュー", answer); err != nil {
		t.Fatal(err)
	}
	saved, err := e.Store.Load(1)
	if err != nil || len(saved.Limits) != 1 {
		t.Fatal(saved, err)
	}
	if err := e.flushUsage(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	if len(s.bodies) != 2 || len(s.bodies[0]["usage"]) != 1 || len(s.bodies[0]["limits"]) != 0 || len(s.bodies[1]["usage"]) != 0 {
		t.Fatalf("使用量と分けて送っていません: %+v", s.bodies)
	}
	rows := sentLimits(s)
	if len(rows) != 1 {
		t.Fatalf("%+v", rows)
	}
	checkIntakeLimit(t, rows[0])
	if rows[0]["limit"] != "seven_day" || rows[0]["percent"] != float64(29) || rows[0]["at"] != "2026-11-03T08:00:00.000Z" {
		t.Fatalf("%+v", rows[0])
	}
	reloaded, err := e.Store.Load(1)
	if err != nil || len(reloaded.Limits) != 0 || reloaded.UsageSent != 1 {
		t.Fatal(reloaded, err)
	}
}

func TestUsageLimitsSendInBatchesWithoutLoss(t *testing.T) {
	s := &limitServer{}
	e, r := limitExecutor(t, s)
	now := time.Now()
	percent := 1.0
	readings := make([]providers.LimitReading, 1201)
	for i := range readings {
		readings[i] = providers.LimitReading{At: now, Provider: "codex", Limit: "seven_day", Percent: &percent}
	}
	r.Limits = limitRows("gt12", readings)
	if err := e.flushUsage(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if len(s.bodies) != 3 || len(sentLimits(s)) != 1201 || len(r.Limits) != 0 {
		t.Fatalf("%d件の要求で%d行", len(s.bodies), len(sentLimits(s)))
	}
	for _, body := range s.bodies {
		if len(body["limits"]) > limitBatchRows {
			t.Fatal(len(body["limits"]))
		}
	}
}

func TestUsageLimitFailuresDoNotStopAndAreRetried(t *testing.T) {
	s := &limitServer{status: http.StatusBadGateway}
	e, r := limitExecutor(t, s)
	r.Limits = limitRows("gt12", []providers.LimitReading{{At: time.Now(), Provider: "claude", Limit: "seven_day"}})
	if err := e.flushUsage(context.Background(), r); err != nil {
		t.Fatalf("送信の失敗で止めています: %v", err)
	}
	if len(r.Limits) != 1 {
		t.Fatal("送れなかった行を失いました")
	}
	s.status = 0
	if err := e.flushUsage(context.Background(), r); err != nil || len(r.Limits) != 0 {
		t.Fatal(r.Limits, err)
	}
}

func TestUsageUnknownReceiptRetainsBatchUntilDuplicateConfirmation(t *testing.T) {
	for name, response := range map[string]string{
		"missing":       `{"added":2,"rejected":0}`,
		"null":          `{"added":2,"duplicate":0,"rejected":null}`,
		"malformed":     `{"added":2,"duplicate":0,"rejected":0} secret`,
		"fraction":      `{"added":2.0,"duplicate":0,"rejected":0}`,
		"quoted":        `{"added":"2","duplicate":0,"rejected":0}`,
		"negative":      `{"added":3,"duplicate":-1,"rejected":0}`,
		"overflow":      `{"added":18446744073709551616,"duplicate":0,"rejected":0}`,
		"sum too large": `{"added":2,"duplicate":1,"rejected":0}`,
		"sum too small": `{"added":1,"duplicate":0,"rejected":0}`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, kind := range []string{"usage", "limits"} {
				t.Run(kind, func(t *testing.T) {
					server := &limitServer{response: response}
					e, r := limitExecutor(t, server)
					rows := []json.RawMessage{encode(map[string]int{"row": 1}), encode(map[string]int{"row": 2})}
					if kind == "usage" {
						r.Usage = rows
					} else {
						r.Limits = rows
					}
					if err := e.Store.Save(r); err != nil {
						t.Fatal(err)
					}
					err := e.flushUsage(t.Context(), r)
					if (err != nil) != (kind == "usage") || (err != nil && strings.Contains(err.Error(), "secret")) {
						t.Fatalf("unknown receipt: %v", err)
					}
					if r.UsageSent != 0 || (kind == "limits" && len(r.Limits) != len(rows)) {
						t.Fatalf("unknown receipt advanced the batch: sent=%d pending limits=%d", r.UsageSent, len(r.Limits))
					}
					reloaded, err := e.Store.Load(r.ID)
					if err != nil {
						t.Fatal(err)
					}
					server.response = `{"added":0,"duplicate":2,"rejected":0}`
					if err := e.flushUsage(t.Context(), reloaded); err != nil {
						t.Fatal(err)
					}
					want := []map[string]any{{"row": float64(1)}, {"row": float64(2)}}
					if len(server.bodies) != 2 || !reflect.DeepEqual(server.bodies[0][kind], want) || !reflect.DeepEqual(server.bodies[0], server.bodies[1]) {
						t.Fatalf("batch changed: %v", server.bodies)
					}
					if reloaded.UsageSent != len(reloaded.Usage) || len(reloaded.Limits) != 0 {
						t.Fatalf("duplicate receipt did not confirm the batch: %+v", reloaded)
					}
				})
			}
		})
	}
}

func TestUsageRejectionStopsBeforeSendingLimits(t *testing.T) {
	server := &limitServer{rejected: 1}
	e, r := limitExecutor(t, server)
	r.Usage = []json.RawMessage{encode(map[string]int{"row": 1}), encode(map[string]int{"row": 2})}
	r.Limits = []json.RawMessage{encode(map[string]int{"row": 3})}
	err := e.flushUsage(t.Context(), r)
	if !errors.Is(err, errUsageRefused) || r.UsageSent != 0 || len(server.bodies) != 1 || len(server.bodies[0]["limits"]) != 0 {
		t.Fatalf("rejected usage: sent=%d requests=%v err=%v", r.UsageSent, server.bodies, err)
	}
}

func TestUsageLimitRejectionIsDroppedWithoutFailingUsage(t *testing.T) {
	s := &limitServer{rejected: 1}
	e, r := limitExecutor(t, s)
	r.Limits = limitRows("gt12", []providers.LimitReading{{At: time.Now(), Provider: "claude", Limit: "seven_day"}})
	if err := e.flushUsage(context.Background(), r); err != nil || len(r.Limits) != 0 {
		t.Fatal(r.Limits, err)
	}
}

func TestLimitRowsLeaveOutRowsTheIntakeCannotAttribute(t *testing.T) {
	now := time.Now()
	if rows := limitRows("", []providers.LimitReading{{At: now, Provider: "claude", Limit: "seven_day"}}); len(rows) != 0 {
		t.Fatal(rows)
	}
	rows := limitRows("gt12", []providers.LimitReading{{At: now, Provider: "claude", Limit: " "}, {At: now, Provider: "codex", Limit: "five_hour"}})
	if len(rows) != 1 {
		t.Fatal(rows)
	}
	var row map[string]any
	if err := json.Unmarshal(rows[0], &row); err != nil {
		t.Fatal(err)
	}
	checkIntakeLimit(t, row)
}

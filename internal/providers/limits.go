package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// limitTailBytes bounds how much of a Codex rollout is read for the last usage sample.
const limitTailBytes = 4 << 20

// limitRolloutDays bounds the fast date-by-date lookup before the full search.
const limitRolloutDays = 31

// Bound the fallback even when the sessions tree contains many unrelated files.
const limitRolloutEntries = 10000

// LimitReading is one observed provider usage window. It carries only numbers and
// labels: prompts, answers, session IDs and credentials never enter it. Values that
// could not be read stay nil rather than being guessed.
type LimitReading struct {
	At                   time.Time
	Provider, Limit      string
	Status, ResetsAt     *string
	Utilization, Percent *float64
	WindowMinutes        *int64
}

func limitTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func limitText(v any) *string {
	if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
		return &s
	}
	return nil
}

func limitNumber(v any) *float64 {
	if n, ok := v.(float64); ok && !math.IsNaN(n) && !math.IsInf(n, 0) {
		return &n
	}
	return nil
}

func limitPercent(utilization *float64) *float64 {
	if utilization == nil {
		return nil
	}
	percent := math.Round(*utilization*100*1e6) / 1e6
	return &percent
}

// limitReset accepts UNIX seconds, UNIX milliseconds (at least 1e12) or an RFC 3339 time.
func limitReset(v any) *string {
	var at time.Time
	switch value := v.(type) {
	case float64:
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil
		}
		if value < 1e12 {
			value *= 1000
		}
		at = time.UnixMilli(int64(value))
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
		if err != nil {
			return nil
		}
		at = parsed
	default:
		return nil
	}
	text := limitTime(at)
	return &text
}

func first(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// ClaudeLimits reads one stream-json line. A rate_limit_event yields a row per
// unified window plus the top-level type when no window covers it. Rows without a
// window name are not reported: the intake cannot attribute them.
func ClaudeLimits(line string, now time.Time) []LimitReading {
	if !strings.Contains(line, "rate_limit_event") {
		return nil
	}
	var event map[string]any
	if json.Unmarshal([]byte(line), &event) != nil || event["type"] != "rate_limit_event" {
		return nil
	}
	info, ok := event["rate_limit_info"].(map[string]any)
	if !ok {
		info = event
	}
	build := func(limit string, status *string, source map[string]any) LimitReading {
		utilization := limitNumber(source["utilization"])
		return LimitReading{At: now, Provider: "claude", Limit: limit, Status: status, Utilization: utilization, Percent: limitPercent(utilization), ResetsAt: limitReset(first(source["resetsAt"], source["resets_at"]))}
	}
	topName := limitText(first(info["rateLimitType"], info["rate_limit_type"]))
	topStatus := limitText(info["status"])
	windows, _ := first(info["unifiedWindows"], info["unified_windows"]).(map[string]any)
	names := make([]string, 0, len(windows))
	for name, window := range windows {
		if _, isObject := window.(map[string]any); isObject && strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	rows := []LimitReading{}
	covered := false
	for _, name := range names {
		window := windows[name].(map[string]any)
		status := limitText(window["status"])
		if topName == nil || name != *topName {
			rows = append(rows, build(name, status, window))
			continue
		}
		covered = true
		if status == nil {
			status = topStatus
		}
		row := build(name, status, window)
		if top := build(name, status, info); top.Utilization != nil {
			row = top
		} else if row.ResetsAt == nil {
			row.ResetsAt = top.ResetsAt
		}
		rows = append(rows, row)
	}
	if !covered && topName != nil {
		rows = append([]LimitReading{build(*topName, topStatus, info)}, rows...)
	}
	return rows
}

func codexHome() string {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return home
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// codexRollout finds the session record, newest day first. A resumed session keeps
// the file of the day it started, so earlier days are searched too.
func codexRollout(home, session string, now time.Time) string {
	if home == "" || !sessionPattern.MatchString(session) {
		return ""
	}
	for day := 1; day >= -limitRolloutDays; day-- {
		for _, at := range []time.Time{now.AddDate(0, 0, day), now.UTC().AddDate(0, 0, day)} {
			matches, _ := filepath.Glob(filepath.Join(home, "sessions", at.Format("2006"), at.Format("01"), at.Format("02"), "*-"+session+".jsonl"))
			if len(matches) > 0 {
				sort.Strings(matches)
				return matches[len(matches)-1]
			}
		}
	}
	// Older resumed sessions retain their original date directory.
	return olderCodexRollout(filepath.Join(home, "sessions"), session)
}

func olderCodexRollout(root, session string) string {
	var latest string
	dirs := []string{root}
	remaining := limitRolloutEntries
	for len(dirs) > 0 {
		dir := dirs[len(dirs)-1]
		dirs = dirs[:len(dirs)-1]
		f, err := os.Open(dir)
		if err != nil {
			return ""
		}
		for {
			entries, err := f.ReadDir(min(remaining+1, 128))
			remaining -= len(entries)
			if remaining < 0 {
				f.Close()
				return ""
			}
			for _, entry := range entries {
				path := filepath.Join(dir, entry.Name())
				if entry.IsDir() {
					dirs = append(dirs, path)
				} else if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), "-"+session+".jsonl") && path > latest {
					latest = path
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				f.Close()
				return ""
			}
		}
		f.Close()
	}
	return latest
}

func readTail(path string) []byte {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	offset := info.Size() - limitTailBytes
	if offset < 0 {
		offset = 0
	}
	data, err := io.ReadAll(io.NewSectionReader(file, offset, info.Size()-offset))
	if err != nil {
		return nil
	}
	if offset > 0 {
		if at := bytes.IndexByte(data, '\n'); at >= 0 {
			return data[at+1:]
		}
		return nil
	}
	return data
}

func codexWindow(value any, now time.Time) (LimitReading, bool) {
	window, ok := value.(map[string]any)
	if !ok {
		return LimitReading{}, false
	}
	minutes := limitNumber(window["window_minutes"])
	if minutes == nil || *minutes < 1 || *minutes != math.Trunc(*minutes) {
		return LimitReading{}, false
	}
	whole := int64(*minutes)
	limit := fmt.Sprintf("window_%dm", whole)
	switch whole {
	case 300:
		limit = "five_hour"
	case 10080:
		limit = "seven_day"
	}
	row := LimitReading{At: now, Provider: "codex", Limit: limit, WindowMinutes: &whole, Percent: limitNumber(window["used_percent"]), ResetsAt: limitReset(window["resets_at"])}
	if row.Percent != nil {
		utilization := *row.Percent / 100
		row.Utilization = &utilization
	}
	if row.ResetsAt == nil {
		if seconds := limitNumber(window["resets_in_seconds"]); seconds != nil && *seconds >= 0 {
			text := limitTime(now.Add(time.Duration(*seconds) * time.Second))
			row.ResetsAt = &text
		}
	}
	return row, true
}

// CodexLimits reads the last token_count with rate limits written at or after start.
// `codex exec --json` has no limit numbers, so the session record is the only source.
// A missing or unreadable record yields no rows; it never fails the run.
func CodexLimits(home, session string, start, now time.Time) []LimitReading {
	path := codexRollout(home, session, now)
	if path == "" {
		return nil
	}
	var latest map[string]any
	for _, line := range bytes.Split(readTail(path), []byte{'\n'}) {
		if !bytes.Contains(line, []byte("token_count")) {
			continue
		}
		var event struct {
			Timestamp string `json:"timestamp"`
			Payload   struct {
				Type       string         `json:"type"`
				RateLimits map[string]any `json:"rate_limits"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &event) != nil || event.Payload.Type != "token_count" || event.Payload.RateLimits == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, event.Timestamp)
		if err != nil || at.Before(start) {
			continue
		}
		latest = event.Payload.RateLimits
	}
	rows := []LimitReading{}
	for _, key := range []string{"primary", "secondary"} {
		if row, ok := codexWindow(latest[key], now); ok {
			rows = append(rows, row)
		}
	}
	return rows
}

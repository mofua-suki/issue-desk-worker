package providers

import (
	"encoding/json"
	"issue-desk-worker/internal/process"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

var usageLimitResetPattern = regexp.MustCompile(`(?i)usage limit reached\|([0-9]{9,13})\b`)
var retryAfterSecondsPattern = regexp.MustCompile(`(?i)retry[ -]after\s*:?\s*([0-9]+(?:\.[0-9]+)?)\s*(?:seconds?|s)?\s*[.!;]?\s*$`)
var retryInPattern = regexp.MustCompile(`(?i)(?:(?:try again|resets?)\s+in|retry[ -]after\s*:?)\s+(.+)`)
var durationPartPattern = regexp.MustCompile(`(?i)^\s*(?:,|and)?\s*([0-9]+(?:\.[0-9]+)?)\s*([a-z]+)`)
var retryAtPattern = regexp.MustCompile(`(?i)(?:try again at|resets?(?:\s+at)?)\s+(.+)`)
var zoneNamePattern = regexp.MustCompile(`\(([A-Za-z][A-Za-z0-9_+:/-]*)\)`)
var parentheticalPattern = regexp.MustCompile(`\s*\([^()]*\)`)
var ordinalSuffixPattern = regexp.MustCompile(`(?i)([0-9])(?:st|nd|rd|th)\b`)
var clauseEndPattern = regexp.MustCompile(`[.!;](?:\s|$)|[∙·|\n]`)
var clockPattern = regexp.MustCompile(`(?i)^([0-9]{1,2})(?::([0-9]{2}))?\s*(am|pm)?$`)
var meridiemPattern = regexp.MustCompile(`(?i)\b(am|pm)\b`)
var authFailurePattern = regexp.MustCompile(`(?i)\b(?:401|403|login|unauthorized)\b|unauthenticated|authentication failed|invalid[_ ](?:api[_ ]key|token)|not logged in`)
var rateLimitPattern = regexp.MustCompile(`(?i)\b429\b|\brate[ _-]limit(?:ed|ing)?(?:\b|_)|too many requests`)
var transientFailurePattern = regexp.MustCompile(`(?i)at capacity|overloaded|\b(?:529|500|502|503|504)\b|service unavailable|bad gateway|gateway timeout|ECONNRESET|ETIMEDOUT|EPIPE|EAI_AGAIN|socket hang up|stream disconnected|connection (?:reset|timed out)|network (?:error|unreachable)|fetch failed`)
var weeklyWindowPattern = regexp.MustCompile(`(?i)seven[ _-]?day|7[ _-]?day|week|週|7日`)
var fiveHourWindowPattern = regexp.MustCompile(`(?i)five[ _-]?hour|5[ _-]?hour|5h\b|五時間|5時間`)

var planLimit = regexp.MustCompile(`(?i)out of credits|add credits to continue|insufficient credits|credit balance is too low|usage limit|quota|利用上限|plan limit|(?:weekly|daily|monthly) limit|(?:you have|you've) (?:hit|reached) your limit`)

func executionError(provider string, r process.Result) (string, bool) {
	text := r.Err
	structuredError := false
	if provider == "codex" || provider == "claude" {
		for _, line := range strings.Split(r.Out, "\n") {
			var event map[string]any
			if json.Unmarshal([]byte(line), &event) != nil {
				continue
			}
			if provider == "codex" {
				if event["type"] == "error" {
					message, _ := event["message"].(string)
					text += "\n" + message
				}
				if event["type"] == "turn.failed" {
					if e, ok := event["error"].(map[string]any); ok {
						message, _ := e["message"].(string)
						text += "\n" + message
					}
				}
			}
			if provider == "claude" && event["type"] == "result" && event["is_error"] == true {
				structuredError = true
				message, _ := event["result"].(string)
				text += "\n" + message
			}
		}
	} else if provider == "antigravity" {
		root := antigravityOutput(r.Out)
		if root != nil {
			var value any
			if raw := root["error"]; len(raw) > 0 && string(raw) != "null" {
				_ = json.Unmarshal(raw, &value)
				switch e := value.(type) {
				case string:
					if strings.TrimSpace(e) != "" {
						structuredError = true
						text += "\n" + e
					}
				case map[string]any:
					structuredError = true
					if message, ok := e["message"].(string); ok {
						text += "\n" + message
					} else if code, ok := e["code"].(string); ok {
						text += "\n" + code
					}
				}
			}
			if !structuredError && (string(root["is_error"]) == "true" || string(root["isError"]) == "true" || string(root["status"]) == `"error"`) {
				structuredError = true
				for _, key := range []string{"result", "response", "message"} {
					var message string
					if json.Unmarshal(root[key], &message) == nil {
						text += "\n" + message
						break
					}
				}
			}
		}
	}
	return text, structuredError
}
func usageFailure(provider string, r process.Result, now time.Time) *UsageLimit {
	text, structuredError := executionError(provider, r)
	if r.Code == 0 && !structuredError || !planLimit.MatchString(text) {
		return nil
	}
	at := readResetAt(text, now)
	source := "cli"
	if !at.After(now) || at.After(now.Add(7*24*time.Hour)) {
		at = now.Add(30 * time.Minute)
		source = "default"
	}
	return &UsageLimit{Provider: provider, Until: at, Source: source, LimitKind: quotaKind(text)}
}
func readResetAt(text string, now time.Time) time.Time {
	if m := usageLimitResetPattern.FindStringSubmatch(text); m != nil {
		v, _ := strconv.ParseInt(m[1], 10, 64)
		if v < 1000000000000 {
			v *= 1000
		}
		return time.UnixMilli(v)
	}
	for _, line := range strings.Split(text, "\n") {
		if m := retryAfterSecondsPattern.FindStringSubmatch(line); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			return now.Add(time.Duration(v * float64(time.Second)))
		}
		if m := retryInPattern.FindStringSubmatch(line); m != nil {
			rest := m[1]
			var total time.Duration
			part := durationPartPattern
			for {
				one := part.FindStringSubmatch(rest)
				if one == nil {
					break
				}
				unit := strings.ToLower(one[2])
				scale := time.Duration(0)
				switch unit {
				case "s", "sec", "secs", "second", "seconds":
					scale = time.Second
				case "m", "min", "mins", "minute", "minutes":
					scale = time.Minute
				case "h", "hr", "hrs", "hour", "hours":
					scale = time.Hour
				case "d", "day", "days":
					scale = 24 * time.Hour
				}
				if scale == 0 {
					break
				}
				v, _ := strconv.ParseFloat(one[1], 64)
				total += time.Duration(v * float64(scale))
				rest = rest[len(one[0]):]
			}
			if total > 0 {
				return now.Add(total)
			}
		}
		if m := retryAtPattern.FindStringSubmatch(line); m != nil {
			if at := absoluteReset(m[1], now); !at.IsZero() {
				return at
			}
		}
	}
	return time.Time{}
}
func absoluteReset(text string, now time.Time) time.Time {
	loc := time.Local
	for _, zone := range zoneNamePattern.FindAllStringSubmatch(text, -1) {
		if strings.EqualFold(zone[1], "at") {
			continue
		}
		var err error
		loc, err = time.LoadLocation(zone[1])
		if err != nil {
			return time.Time{}
		}
		break
	}
	text = parentheticalPattern.ReplaceAllString(text, " ")
	text = ordinalSuffixPattern.ReplaceAllString(text, "${1}")
	text = clauseEndPattern.Split(text, 2)[0]
	text = strings.Join(strings.Fields(text), " ")
	if m := clockPattern.FindStringSubmatch(text); m != nil {
		if m[2] == "" && m[3] == "" {
			return time.Time{}
		}
		hour, _ := strconv.Atoi(m[1])
		minute, _ := strconv.Atoi(m[2])
		mer := strings.ToLower(m[3])
		if minute > 59 || mer != "" && (hour < 1 || hour > 12) || mer == "" && hour > 23 {
			return time.Time{}
		}
		if mer != "" {
			hour %= 12
			if mer == "pm" {
				hour += 12
			}
		}
		n := now.In(loc)
		at := time.Date(n.Year(), n.Month(), n.Day(), hour, minute, 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		return at
	}
	if at, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return at
	}
	normalized := meridiemPattern.ReplaceAllStringFunc(text, strings.ToUpper)
	layouts := []string{"Jan 2, 2006 3:04 PM", "Jan 2, 2006 3 PM", "Jan 2 2006 3:04 PM", "January 2, 2006 3:04 PM", "2006-01-02 15:04:05", "2006-01-02 15:04", "Jan 2 3:04 PM", "Jan 2 3PM", "Jan 2 15:04", "Jan 2, 3:04 PM"}
	for _, layout := range layouts {
		if at, err := time.ParseInLocation(layout, normalized, loc); err == nil {
			if !strings.Contains(layout, "2006") {
				at = time.Date(now.In(loc).Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), 0, 0, loc)
				if !at.After(now) {
					at = at.AddDate(1, 0, 0)
				}
			}
			return at
		}
	}
	return time.Time{}
}

func transientFailure(provider string, r process.Result) bool {
	text, structured := executionError(provider, r)
	if r.Code == 0 && !structured || planLimit.MatchString(text) {
		return false
	}
	if authFailurePattern.MatchString(text) {
		return false
	}
	if rateLimitPattern.MatchString(text) {
		return true
	}
	return provider != "antigravity" && transientFailurePattern.MatchString(text)
}

func quotaKind(text string) string { return QuotaKind(text) }

func QuotaKind(text string) string {
	if weeklyWindowPattern.MatchString(text) {
		return "seven_day"
	}
	if fiveHourWindowPattern.MatchString(text) {
		return "five_hour"
	}
	return "unknown"
}

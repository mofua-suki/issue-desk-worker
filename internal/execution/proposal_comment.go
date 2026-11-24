package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

var proposalMarker = regexp.MustCompile(`<!-- issue-desk-proposal (\{[^<>]*\}) -->`)
var proposalID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var proposalSource = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[1-9][0-9]{0,15}$`)

// Only the identity is needed by reposting; the entire wire object is validated.
type proposalComment struct{ ID, Source string }

func utf16Length(s string) int { return len(utf16.Encode([]rune(s))) }

// JSON numbers such as 1.0 and 1e0 represent the same integer on the wire.
func proposalInteger(raw json.RawMessage, min, max int64) (int64, bool) {
	var n float64
	if json.Unmarshal(raw, &n) != nil || n < float64(min) || n > float64(max) {
		return 0, false
	}
	// Check the exact decimal too: a fractional ID near 2^53 must not round
	// into an integer while being decoded as float64.
	exact, ok := new(big.Rat).SetString(strings.TrimSpace(string(raw)))
	if !ok || !exact.IsInt() || !exact.Num().IsInt64() {
		return 0, false
	}
	integer := exact.Num().Int64()
	return integer, integer >= min && integer <= max
}
func validProposalSource(s string) bool {
	if !proposalSource.MatchString(s) {
		return false
	}
	n, err := strconv.ParseInt(s[strings.LastIndexByte(s, '#')+1:], 10, 64)
	return err == nil && n <= 9007199254740991
}
func proposalObject(raw json.RawMessage, names ...string) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil || len(m) != len(names) {
		return nil, false
	}
	for _, k := range names {
		if _, ok := m[k]; !ok {
			return nil, false
		}
	}
	return m, true
}
func proposalString(raw json.RawMessage, limit int) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, utf16Length(s) <= limit
}
func proposalStrings(raw json.RawMessage, min, max, limit int) ([]string, bool) {
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil || rows == nil || len(rows) < min || len(rows) > max {
		return nil, false
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		s, ok := proposalString(row, limit)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}
func parseProposalComment(body string) (proposalComment, error) {
	refuse := func(reason string) (proposalComment, error) { return proposalComment{}, errors.New(reason) }
	if strings.Count(body, "<!-- issue-desk-proposal") != 1 {
		return refuse("proposal_marker_count")
	}
	matches := proposalMarker.FindStringSubmatch(body)
	if matches == nil {
		return refuse("proposal_marker_format")
	}
	// HTML-sensitive characters must be JSON escapes, not literal marker content.
	if strings.ContainsAny(matches[1], "<>&") {
		return refuse("proposal_escape")
	}
	raw := json.RawMessage(matches[1])
	if !json.Valid(raw) {
		return refuse("proposal_json")
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return refuse("proposal_json")
	}
	version, validVersion := proposalInteger(root["version"], 1, 3)
	if !validVersion {
		return refuse("proposal_version")
	}
	names := []string{"version", "kind", "id", "source", "title", "purpose", "acceptance", "dependsOn", "provider"}
	if version == 2 {
		names = append(names, "changeScope")
	}
	if version == 3 {
		names = []string{"version", "kind", "id", "source", "title", "pr", "items", "dependsOn", "provider", "changeScope"}
	}
	if version == 3 {
		if _, exists := root["priority"]; exists {
			names = append(names, "priority")
		}
	}
	m, ok := proposalObject(raw, names...)
	if !ok {
		return refuse("proposal_fields")
	}
	id, ok := proposalString(m["id"], 128)
	if !ok || !proposalID.MatchString(id) {
		return refuse("proposal_id")
	}
	source, ok := proposalString(m["source"], int(^uint(0)>>1))
	if !ok || !validProposalSource(source) {
		return refuse("proposal_source")
	}
	title, ok := proposalString(m["title"], 200)
	if !ok || title == "" || strings.ContainsAny(title, "\r\n") {
		return refuse("proposal_title")
	}
	kind, ok := proposalString(m["kind"], 20)
	if !ok || (kind != "followup" && kind != "improvements" && kind != "split") || version == 3 && kind != "followup" {
		return refuse("proposal_kind")
	}
	provider, ok := proposalString(m["provider"], 10)
	if !ok || (provider != "codex" && provider != "claude") {
		return refuse("proposal_provider")
	}
	deps, ok := proposalStrings(m["dependsOn"], 0, 30, int(^uint(0)>>1))
	if !ok {
		return refuse("proposal_depends_on")
	}
	seen := map[string]bool{}
	for _, dep := range deps {
		if !validProposalSource(dep) || seen[dep] {
			return refuse("proposal_depends_on")
		}
		seen[dep] = true
	}
	if version < 3 {
		if _, ok := proposalString(m["purpose"], 8000); !ok {
			return refuse("proposal_purpose")
		}
		if _, ok := proposalStrings(m["acceptance"], 1, 30, 2000); !ok {
			return refuse("proposal_acceptance")
		}
	} else {
		if priority, exists := m["priority"]; exists {
			fields, valid := proposalObject(priority, "level", "reason")
			if !valid {
				return refuse("proposal_priority")
			}
			level, levelOK := proposalString(fields["level"], 10)
			var reason string
			if !levelOK || level != "high" || json.Unmarshal(fields["reason"], &reason) != nil || !utf8.ValidString(reason) || strings.TrimSpace(reason) == "" || utf8.RuneCountInString(strings.TrimSpace(reason)) > 500 {
				return refuse("proposal_priority")
			}
		}
		pr, ok := proposalString(m["pr"], 500)
		u, err := url.Parse(pr)
		if !ok || err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || strings.ContainsFunc(pr, unicode.IsSpace) {
			return refuse("proposal_pr")
		}
		var items []json.RawMessage
		if json.Unmarshal(m["items"], &items) != nil || len(items) < 1 || len(items) > 5 {
			return refuse("proposal_items")
		}
		for _, item := range items {
			fields, ok := proposalObject(item, "problem", "expected", "test")
			if !ok {
				return refuse("proposal_items")
			}
			for _, field := range fields {
				if _, ok := proposalString(field, 2000); !ok {
					return refuse("proposal_items")
				}
			}
		}
	}
	if version >= 2 && !(version == 3 && string(m["changeScope"]) == "null") {
		scope, ok := proposalObject(m["changeScope"], "paths")
		if !ok {
			return refuse("proposal_change_scope")
		}
		if _, ok := proposalStrings(scope["paths"], 1, 40, 200); !ok {
			return refuse("proposal_change_scope")
		}
	}
	return proposalComment{ID: id, Source: source}, nil
}

var trailingGCR = regexp.MustCompile(`<!-- gcr:[^<>\r\n]* -->([ \t\r\n]*)$`)

func repostBody(original string, commentID int64) string {
	// Preserve all text and whitespace except the last trailing gcr marker itself.
	loc := trailingGCR.FindStringSubmatchIndex(original)
	if loc != nil {
		original = original[:loc[0]] + original[loc[2]:]
	}
	return original + fmt.Sprintf("\n\n<!-- issue-desk-repost:%d -->", commentID)
}

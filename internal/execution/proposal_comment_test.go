package execution

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func proposalFixture(version int) map[string]any {
	p := map[string]any{"version": version, "kind": "followup", "id": "proposal_1", "source": "owner/project#7", "title": "後続案", "purpose": "目的", "acceptance": []string{"確認"}, "dependsOn": []string{}, "provider": "codex"}
	if version >= 2 {
		p["changeScope"] = map[string]any{"paths": []string{"internal/example.go"}}
	}
	if version == 3 {
		delete(p, "purpose")
		delete(p, "acceptance")
		p["pr"] = "https://example.test/pulls/1"
		p["items"] = []any{map[string]any{"problem": "問題", "expected": "期待", "test": "検証"}}
		p["changeScope"] = nil
	}
	return p
}
func proposalText(p map[string]any) string {
	return "本文\n<!-- issue-desk-proposal " + string(encode(p)) + " -->"
}
func stringRows(n, size int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = strings.Repeat("a", size)
	}
	return rows
}
func dependencyRows(n int) []string {
	rows := make([]string, n)
	for i := range rows {
		rows[i] = fmt.Sprintf("owner/project#%d", i+1)
	}
	return rows
}

func TestProposalCommentValidationTable(t *testing.T) {
	cases := []struct {
		name    string
		version int
		mutate  func(map[string]any)
		reason  string
	}{
		{"priority-valid", 3, func(p map[string]any) {
			p["priority"] = map[string]any{"level": "high", "reason": "  " + strings.Repeat("😀", 500) + "  "}
		}, ""},
		{"priority-empty", 3, func(p map[string]any) { p["priority"] = map[string]any{"level": "high", "reason": "  "} }, "proposal_priority"},
		{"priority-long", 3, func(p map[string]any) {
			p["priority"] = map[string]any{"level": "high", "reason": strings.Repeat("😀", 501)}
		}, "proposal_priority"},
		{"priority-level", 3, func(p map[string]any) { p["priority"] = map[string]any{"level": "low", "reason": "x"} }, "proposal_priority"},
		{"priority-extra", 3, func(p map[string]any) { p["priority"] = map[string]any{"level": "high", "reason": "x", "extra": true} }, "proposal_priority"},
		{"priority-v2", 2, func(p map[string]any) { p["priority"] = map[string]any{"level": "high", "reason": "x"} }, "proposal_fields"},
		{"v1", 1, nil, ""}, {"v2", 2, nil, ""}, {"v3-null-scope", 3, nil, ""},
		{"v3-path-scope", 3, func(p map[string]any) { p["changeScope"] = map[string]any{"paths": []string{"x"}} }, ""},
		{"claude-split", 2, func(p map[string]any) { p["provider"] = "claude"; p["kind"] = "split" }, ""},
		{"improvements", 1, func(p map[string]any) { p["kind"] = "improvements" }, ""},
		{"unsupported-version", 1, func(p map[string]any) { p["version"] = 4 }, "proposal_version"},
		{"fractional-version", 1, func(p map[string]any) { p["version"] = 1.1 }, "proposal_version"},
		{"string-version", 1, func(p map[string]any) { p["version"] = "1" }, "proposal_version"},
		{"extra-field", 1, func(p map[string]any) { p["extra"] = true }, "proposal_fields"},
		{"missing-field", 1, func(p map[string]any) { delete(p, "title") }, "proposal_fields"},
		{"v1-scope", 1, func(p map[string]any) { p["changeScope"] = nil }, "proposal_fields"},
		{"v3-purpose", 3, func(p map[string]any) { p["purpose"] = "x" }, "proposal_fields"},
		{"id-limit", 1, func(p map[string]any) { p["id"] = strings.Repeat("a", 128) }, ""},
		{"id-over", 1, func(p map[string]any) { p["id"] = strings.Repeat("a", 129) }, "proposal_id"},
		{"id-empty", 1, func(p map[string]any) { p["id"] = "" }, "proposal_id"},
		{"id-characters", 1, func(p map[string]any) { p["id"] = "案" }, "proposal_id"},
		{"id-type", 1, func(p map[string]any) { p["id"] = 1 }, "proposal_id"},
		{"source-max", 1, func(p map[string]any) { p["source"] = "a.b-c/d_e#9007199254740991" }, ""},
		{"source-over", 1, func(p map[string]any) { p["source"] = "a/b#9007199254740992" }, "proposal_source"},
		{"source-leading-zero", 1, func(p map[string]any) { p["source"] = "a/b#01" }, "proposal_source"},
		{"source-zero", 1, func(p map[string]any) { p["source"] = "a/b#0" }, "proposal_source"},
		{"source-long-number", 1, func(p map[string]any) { p["source"] = "a/b#10000000000000000" }, "proposal_source"},
		{"source-type", 1, func(p map[string]any) { p["source"] = nil }, "proposal_source"},
		{"title-utf16-limit", 1, func(p map[string]any) { p["title"] = strings.Repeat("😀", 100) }, ""},
		{"title-utf16-over", 1, func(p map[string]any) { p["title"] = strings.Repeat("😀", 100) + "a" }, "proposal_title"},
		{"title-empty", 1, func(p map[string]any) { p["title"] = "" }, "proposal_title"},
		{"title-newline", 1, func(p map[string]any) { p["title"] = "a\nb" }, "proposal_title"},
		{"title-cr", 1, func(p map[string]any) { p["title"] = "a\rb" }, "proposal_title"},
		{"title-type", 1, func(p map[string]any) { p["title"] = false }, "proposal_title"},
		{"kind", 1, func(p map[string]any) { p["kind"] = "other" }, "proposal_kind"},
		{"v3-kind", 3, func(p map[string]any) { p["kind"] = "split" }, "proposal_kind"},
		{"provider", 1, func(p map[string]any) { p["provider"] = "other" }, "proposal_provider"},
		{"depends-max", 1, func(p map[string]any) { p["dependsOn"] = dependencyRows(30) }, ""},
		{"depends-over", 1, func(p map[string]any) { p["dependsOn"] = dependencyRows(31) }, "proposal_depends_on"},
		{"depends-duplicate", 1, func(p map[string]any) { p["dependsOn"] = []string{"a/b#1", "a/b#1"} }, "proposal_depends_on"},
		{"depends-source", 1, func(p map[string]any) { p["dependsOn"] = []string{"a/b#9007199254740992"} }, "proposal_depends_on"},
		{"depends-type", 1, func(p map[string]any) { p["dependsOn"] = []any{1} }, "proposal_depends_on"},
		{"depends-null", 1, func(p map[string]any) { p["dependsOn"] = nil }, "proposal_depends_on"},
		{"purpose-max", 1, func(p map[string]any) { p["purpose"] = strings.Repeat("😀", 4000) }, ""},
		{"purpose-over", 2, func(p map[string]any) { p["purpose"] = strings.Repeat("a", 8001) }, "proposal_purpose"},
		{"purpose-type", 1, func(p map[string]any) { p["purpose"] = nil }, "proposal_purpose"},
		{"acceptance-max", 2, func(p map[string]any) { p["acceptance"] = stringRows(30, 2000) }, ""},
		{"acceptance-over", 1, func(p map[string]any) { p["acceptance"] = stringRows(31, 1) }, "proposal_acceptance"},
		{"acceptance-long", 1, func(p map[string]any) { p["acceptance"] = stringRows(1, 2001) }, "proposal_acceptance"},
		{"acceptance-empty", 1, func(p map[string]any) { p["acceptance"] = []string{} }, "proposal_acceptance"},
		{"acceptance-type", 1, func(p map[string]any) { p["acceptance"] = []any{1} }, "proposal_acceptance"},
		{"scope-max", 2, func(p map[string]any) { p["changeScope"] = map[string]any{"paths": stringRows(40, 200)} }, ""},
		{"scope-over", 2, func(p map[string]any) { p["changeScope"] = map[string]any{"paths": stringRows(41, 1)} }, "proposal_change_scope"},
		{"scope-long", 2, func(p map[string]any) { p["changeScope"] = map[string]any{"paths": stringRows(1, 201)} }, "proposal_change_scope"},
		{"scope-empty", 2, func(p map[string]any) { p["changeScope"] = map[string]any{"paths": []string{}} }, "proposal_change_scope"},
		{"scope-null", 2, func(p map[string]any) { p["changeScope"] = nil }, "proposal_change_scope"},
		{"scope-extra", 2, func(p map[string]any) { p["changeScope"] = map[string]any{"paths": []string{"x"}, "extra": true} }, "proposal_change_scope"},
		{"scope-type", 2, func(p map[string]any) { p["changeScope"] = map[string]any{"paths": []any{nil}} }, "proposal_change_scope"},
		{"pr-max", 3, func(p map[string]any) { p["pr"] = "http://a/" + strings.Repeat("a", 491) }, ""},
		{"pr-over", 3, func(p map[string]any) { p["pr"] = "http://a/" + strings.Repeat("a", 492) }, "proposal_pr"},
		{"pr-space", 3, func(p map[string]any) { p["pr"] = "https://a/b c" }, "proposal_pr"},
		{"pr-unicode-space", 3, func(p map[string]any) { p["pr"] = "https://a/b\u2003c" }, "proposal_pr"},
		{"pr-scheme", 3, func(p map[string]any) { p["pr"] = "ftp://a/b" }, "proposal_pr"},
		{"pr-no-host", 3, func(p map[string]any) { p["pr"] = "https:///path" }, "proposal_pr"},
		{"pr-type", 3, func(p map[string]any) { p["pr"] = nil }, "proposal_pr"},
		{"items-max", 3, func(p map[string]any) {
			rows := []any{}
			for i := 0; i < 5; i++ {
				rows = append(rows, map[string]any{"problem": strings.Repeat("a", 2000), "expected": strings.Repeat("😀", 1000), "test": strings.Repeat("a", 2000)})
			}
			p["items"] = rows
		}, ""},
		{"items-over", 3, func(p map[string]any) { p["items"] = make([]any, 6) }, "proposal_items"},
		{"items-empty", 3, func(p map[string]any) { p["items"] = []any{} }, "proposal_items"},
		{"items-extra", 3, func(p map[string]any) {
			p["items"] = []any{map[string]any{"problem": "x", "expected": "x", "test": "x", "extra": "x"}}
		}, "proposal_items"},
		{"items-missing", 3, func(p map[string]any) { p["items"] = []any{map[string]any{"problem": "x", "test": "x"}} }, "proposal_items"},
		{"items-type", 3, func(p map[string]any) {
			p["items"] = []any{map[string]any{"problem": nil, "expected": "x", "test": "x"}}
		}, "proposal_items"},
		{"items-long", 3, func(p map[string]any) {
			p["items"] = []any{map[string]any{"problem": strings.Repeat("a", 2001), "expected": "x", "test": "x"}}
		}, "proposal_items"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := proposalFixture(tc.version)
			if tc.mutate != nil {
				tc.mutate(p)
			}
			got, err := parseProposalComment(proposalText(p))
			if tc.reason == "" {
				if err != nil || got.ID != p["id"] {
					t.Fatalf("%+v %v", got, err)
				}
			} else if err == nil || err.Error() != tc.reason {
				t.Fatalf("got %v, want %s", err, tc.reason)
			}
		})
	}
}
func TestProposalMarkerEscapesAndFinalBody(t *testing.T) {
	p := proposalFixture(1)
	p["purpose"] = "< > &"
	body := proposalText(p)
	cases := []struct{ name, body, reason string }{
		{"escaped", body, ""},
		{"none", "本文", "proposal_marker_count"},
		{"double", body + body, "proposal_marker_count"},
		{"prefix-collision", body + "<!-- issue-desk-proposal-repost -->", "proposal_marker_count"},
		{"malformed", strings.Replace(body, " -->", "-->", 1), "proposal_marker_format"},
		{"json", "<!-- issue-desk-proposal {bad} -->", "proposal_json"},
		{"ampersand", strings.ReplaceAll(body, `\u0026`, "&"), "proposal_escape"},
		{"less-than", strings.ReplaceAll(body, `\u003c`, "<"), "proposal_marker_format"},
		{"greater-than", strings.ReplaceAll(body, `\u003e`, ">"), "proposal_marker_format"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProposalComment(tc.body)
			if tc.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || err.Error() != tc.reason {
				t.Fatalf("%v want %s", err, tc.reason)
			}
		})
	}
	original := body + "\n<!-- gcr:middle -->\n保持\n<!-- gcr:last --> \n"
	final := repostBody(original, 42)
	want := body + "\n<!-- gcr:middle -->\n保持\n \n\n\n<!-- issue-desk-repost:42 -->"
	if final != want {
		t.Fatalf("body changed: %q", final)
	}
	if _, err := parseProposalComment(final); err != nil {
		t.Fatal(err)
	}
	if strings.Count(final, "<!-- issue-desk-proposal") != 1 {
		t.Fatal("marker collision")
	}
	unchanged := body + "\n<!-- gcr:not-last -->\nafter"
	if repostBody(unchanged, 42) != unchanged+"\n\n<!-- issue-desk-repost:42 -->" {
		t.Fatal("removed a nontrailing marker")
	}
	var decoded map[string]any
	if json.Unmarshal(encode(p), &decoded) != nil {
		t.Fatal("invalid fixture")
	}
}

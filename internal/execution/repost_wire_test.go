package execution

import (
	"encoding/json"
	"testing"
)

func TestRepostMalformedWireValuesAreRefused(t *testing.T) {
	cases := []struct {
		name, field string
		value       any
		reason      string
	}{
		{"payload-string", "payload", "bad", "payload_version"},
		{"payload-array", "payload", []any{}, "payload_version"},
		{"payload-null", "payload", nil, "payload_version"},
		{"repo-type", "repo", 123, "repository_not_assigned"},
		{"target-type", "targetHost", 123, "target_host"},
		{"issue-type", "issue", true, "issue_number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepostFixture(t)
			var row map[string]any
			if err := json.Unmarshal(encode(f.commands[0]), &row); err != nil {
				t.Fatal(err)
			}
			row[tc.field] = tc.value
			f.wireCommands = []any{row}
			f.poll(t)
			f.last(t, "failed", "refused", tc.reason)
			if f.posts != 0 {
				t.Fatal("malformed command posted")
			}
		})
	}
}

func TestRepostNumbersDoNotRoundFractionalIDs(t *testing.T) {
	for _, raw := range []string{`9007199254740990.5`, `1.00000000000000001`, `null`, `"1"`, `true`} {
		if _, valid := positiveCommandNumber(json.RawMessage(raw)); valid {
			t.Errorf("accepted noninteger %s", raw)
		}
	}
}

func TestRepostCommentIDInt64Boundary(t *testing.T) {
	for _, raw := range []string{`1`, `9007199254740992`, `9223372036854775807`} {
		if _, valid := positiveCommandNumber(json.RawMessage(raw)); !valid {
			t.Errorf("refused positive Gitea ID %s", raw)
		}
	}
	for _, raw := range []string{`0`, `-1`, `9223372036854775808`} {
		if _, valid := positiveCommandNumber(json.RawMessage(raw)); valid {
			t.Errorf("accepted invalid Gitea ID %s", raw)
		}
	}
}

package fleetconfig

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"issue-desk-worker/internal/intake"
)

func TestDecodeConfigIgnoresReviewRoundLimit(t *testing.T) {
	for _, tc := range []struct {
		name, field string
	}{
		{"omitted", ""},
		{"zero", `,"reviewRoundLimit":0`},
		{"negative", `,"reviewRoundLimit":-1`},
		{"above former range", `,"reviewRoundLimit":101`},
		{"large", `,"reviewRoundLimit":2147483647`},
		{"overflow", `,"reviewRoundLimit":1e20`},
		{"fraction", `,"reviewRoundLimit":2.5`},
		{"string", `,"reviewRoundLimit":"x"`},
		{"object", `,"reviewRoundLimit":{"nested":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := fmt.Sprintf(`{"git":"git","codex":"codex","giteaUrl":"https://example.invalid","runtimeDir":"/tmp/runtime","workerTokenFile":"token","assignments":{"implementationRunLimit":4,"implementationUsageLimit":8000000%s}}`, tc.field)
			got, err := Decode(intake.ConfigSnapshot{Config: json.RawMessage(config)}, "host")
			if err != nil {
				t.Fatal(err)
			}
			if got.Assignments.ImplementationRunLimit != 4 || got.Assignments.ImplementationUsageLimit != 8000000 {
				t.Fatalf("unexpected assignments: %+v", got.Assignments)
			}
		})
	}
}

func TestReviewNotesBoundsAndInvalidShapes(t *testing.T) {
	config := json.RawMessage(`{"git":"git","codex":"codex","giteaUrl":"https://example.invalid","runtimeDir":"/tmp/runtime","workerTokenFile":"token"}`)
	cases := []string{`null`, `[]`, `{"r":{}}`, `{"r":[{"kind":"x"}]}`, `{"r":[null]}`, `{"r":[{"kind":"x","text":4}]}`, `{"r":[{"kind":"x","text":"有効"},{"kind":3,"text":"不正"}]}`}
	for _, raw := range cases {
		c, err := Decode(intake.ConfigSnapshot{Config: config, ReviewNotes: json.RawMessage(raw)}, "host")
		if err != nil || len(c.ReviewNotesFor("r")) != 0 {
			t.Fatalf("%s: %v", raw, err)
		}
	}
	notes := make([]map[string]string, 6)
	for i := range notes {
		notes[i] = map[string]string{"kind": "種類", "text": strings.Repeat("あ", 1500)}
	}
	raw, _ := json.Marshal(map[string]any{"r": notes})
	c, err := Decode(intake.ConfigSnapshot{Config: config, ReviewNotes: raw}, "host")
	if err != nil {
		t.Fatal(err)
	}
	got := c.ReviewNotesFor("r")
	total := 0
	for _, n := range got {
		size := len([]rune(n.Kind)) + len([]rune(n.Text))
		if size > 1000 {
			t.Fatal(size)
		}
		total += size
	}
	if len(got) != 4 || total != 4000 || len([]rune(got[0].Text)) != 998 {
		t.Fatal(got, total)
	}
}

// 書き間違えた役割は、実装の仕事を引き受けずに断る。正しい役割（空・implementation）は通す。
func TestDecodeRefusesUnknownHostRole(t *testing.T) {
	for role, ok := range map[string]bool{"": true, "implementation": true, "test_only": false, "Test-Only": false} {
		raw := json.RawMessage(`{"git":"/usr/bin/git","codex":"/usr/bin/codex","giteaUrl":"https://git.invalid","runtimeDir":"/runtime","workerTokenFile":"/token","assignments":{"hostRoles":{"pc":"` + role + `"}}}`)
		_, err := Decode(intake.ConfigSnapshot{Version: "v", ConfigurationVersion: "c", SettingsVersion: "s", Config: raw}, "pc")
		if ok && err != nil || !ok && (err == nil || !strings.Contains(err.Error(), role)) {
			t.Errorf("%q: %v", role, err)
		}
	}
}

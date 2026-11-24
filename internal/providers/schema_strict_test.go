package providers

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAssumptionsMinimumIsPromptOnly(t *testing.T) {
	var prompt, receive map[string]any
	_ = json.Unmarshal(Schema(false), &prompt)
	_ = json.Unmarshal(build(false, false), &receive)
	presented := prompt["properties"].(map[string]any)["assumptions"].(map[string]any)
	accepted := receive["properties"].(map[string]any)["assumptions"].(map[string]any)
	if presented["minItems"] != float64(1) {
		t.Fatal("missing prompt minimum")
	}
	if _, exists := accepted["minItems"]; exists {
		t.Fatal("receive rejects repairable empty array")
	}
	if err := Validate([]byte(emptyAssumptionsReport), false); err != nil {
		t.Fatal(err)
	}
	if err := ValidateReport([]byte(emptyAssumptionsReport), false); err == nil {
		t.Fatal("bypassed report validation")
	}
	// Validate every nested object against the strict all-keys-required invariant.
	var check func(map[string]any)
	check = func(schema map[string]any) {
		if props, ok := schema["properties"].(map[string]any); ok {
			required := map[string]bool{}
			for _, key := range schema["required"].([]any) {
				required[key.(string)] = true
			}
			for key, raw := range props {
				if !required[key] {
					t.Error("strict missing", key)
				}
				check(raw.(map[string]any))
			}
		}
		if items, ok := schema["items"].(map[string]any); ok {
			check(items)
		}
	}
	check(prompt)
}

func TestReviewSchemaRequiresEveryProperty(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(Schema(true), &schema); err != nil {
		t.Fatal(err)
	}
	var check func(map[string]any)
	check = func(s map[string]any) {
		if properties, ok := s["properties"].(map[string]any); ok {
			required := map[string]bool{}
			for _, name := range s["required"].([]any) {
				required[name.(string)] = true
			}
			for name, property := range properties {
				if !required[name] {
					t.Errorf("requiredに %q がありません", name)
				}
				check(property.(map[string]any))
			}
		}
		if items, ok := s["items"].(map[string]any); ok {
			check(items)
		}
	}
	if _, ok := schema["properties"].(map[string]any)["recordOnly"]; !ok {
		t.Fatal("recordOnly がありません")
	}
	check(schema)
}

func TestReviewValidateRecordOnlyCompatibility(t *testing.T) {
	const legacy = `{"decision":"approve","summary":"確認","breaking":false,"tooLarge":false,"findings":["指摘"],"findingKinds":["other"],"findingSeverities":["minor"],"findingSpecs":[],"followups":[],"followupSpecs":[],"improvements":[],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":[]}`
	var review map[string]any
	if err := json.Unmarshal([]byte(legacy), &review); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		value   any
		include bool
		valid   bool
	}{
		{"legacy", nil, false, true},
		{"strings", []any{"指摘"}, true, true},
		{"empty", []any{}, true, true},
		{"scalar", "指摘", true, false},
		{"non-string item", []any{1}, true, false},
		{"null", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.include {
				review["recordOnly"] = tc.value
			} else {
				delete(review, "recordOnly")
			}
			data, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(data, true); (err == nil) != tc.valid {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestReviewValidateRejectsMissingRequiredFields(t *testing.T) {
	const legacy = `{"decision":"approve","summary":"確認","breaking":false,"tooLarge":false,"findings":[],"findingKinds":[],"findingSeverities":[],"findingSpecs":[],"followups":[],"followupSpecs":[],"improvements":[],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":[]}`
	var schema struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(Schema(true), &schema); err != nil {
		t.Fatal(err)
	}
	var review map[string]any
	if err := json.Unmarshal([]byte(legacy), &review); err != nil {
		t.Fatal(err)
	}
	if err := Validate([]byte(legacy), true); err != nil {
		t.Fatalf("旧形式を拒否しました: %v", err)
	}
	for _, name := range schema.Required {
		if name == "recordOnly" || name == "previousFindings" || name == "interrupts" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			value, exists := review[name]
			if !exists {
				t.Fatalf("検査用レビューに %s がありません", name)
			}
			delete(review, name)
			defer func() { review[name] = value }()
			data, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			if err := Validate(data, true); err == nil {
				t.Fatalf("必須欄 %s の欠落を受け入れました", name)
			}
		})
	}
}

func TestScopeExtensionsReceiveCompatibility(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal(Schema(false), &schema); err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal([]byte(emptyAssumptionsReport), &legacy); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name           string
		value          any
		present, valid bool
	}{
		{"omitted", nil, false, true}, {"empty", []any{}, true, true},
		{"entry", []any{map[string]any{"path": "x.go", "reason": "required"}}, true, true},
		{"null", nil, true, false}, {"wrong type", "x", true, false},
		{"item type", []any{"x"}, true, false}, {"missing reason", []any{map[string]any{"path": "x"}}, true, false},
		{"missing path", []any{map[string]any{"reason": "x"}}, true, false},
		{"empty path", []any{map[string]any{"path": "", "reason": "x"}}, true, false},
		{"empty reason", []any{map[string]any{"path": "x", "reason": ""}}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delete(legacy, "scopeExtensions")
			if tc.present {
				legacy["scopeExtensions"] = tc.value
			}
			b, _ := json.Marshal(legacy)
			if err := Validate(b, false); (err == nil) != tc.valid {
				t.Fatalf("receive: %v", err)
			}
			// The presented schema has a separate assumptions minimum; supply valid data for it.
			legacy["deleteFiles"] = []any{}
			legacy["addDependencies"] = []any{}
			legacy["assumptions"] = []any{map[string]any{"assumption": "a", "evidence": "b", "verified": true}}
			b, _ = json.Marshal(legacy)
			var v any
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.UseNumber()
			_ = dec.Decode(&v)
			if err := validate(schema, v, "result"); (err == nil) != (tc.valid && tc.present) {
				t.Fatalf("presented: %v", err)
			}
		})
	}
}

func TestReviewFindingClassificationCompatibility(t *testing.T) {
	const prefix = `{"decision":"approve","summary":"ok","breaking":false,"tooLarge":false,"findings":["x"],"findingKinds":["other"],"findingSeverities":["minor"],"findingSpecs":[{"group":"other","problem":"x","expected":"x","test":"x"`
	const suffix = `}],"followups":[],"followupSpecs":[],"improvements":[],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":[]}`
	for _, tc := range []struct {
		name, field string
		valid       bool
	}{{"legacy", "", true}, {"external", `,"externalCheckOnly":true`, true}, {"actionable", `,"externalCheckOnly":false`, true}, {"invalid", `,"externalCheckOnly":"true"`, false}} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate([]byte(prefix+tc.field+suffix), true); (err == nil) != tc.valid {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestReviewPreviousFindingsCompatibility(t *testing.T) {
	const legacy = `{"decision":"approve","summary":"確認","breaking":false,"tooLarge":false,"findings":[],"findingKinds":[],"findingSeverities":[],"findingSpecs":[],"followups":[],"followupSpecs":[],"improvements":[],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":[]}`
	var value map[string]any
	if err := json.Unmarshal([]byte(legacy), &value); err != nil {
		t.Fatal(err)
	}
	if err := Validate([]byte(legacy), true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		links any
		valid bool
	}{
		{[]any{map[string]any{"index": 0, "status": "unresolved", "current": 0}}, true},
		{[]any{map[string]any{"index": 0, "status": "resolved", "current": nil}}, true},
		{[]any{map[string]any{"index": 0, "status": "resolved"}}, false},
		{[]any{map[string]any{"index": "0", "status": "unresolved", "current": 0}}, false},
	} {
		value["previousFindings"] = tc.links
		raw, _ := json.Marshal(value)
		if err := Validate(raw, true); (err == nil) != tc.valid {
			t.Fatalf("links=%v err=%v", tc.links, err)
		}
	}
}

func TestReviewValidateInterruptCompatibility(t *testing.T) {
	const legacy = `{"decision":"approve","summary":"確認","breaking":false,"tooLarge":false,"findings":[],"findingKinds":[],"findingSeverities":[],"findingSpecs":[],"followups":[],"followupSpecs":[],"improvements":[],"postMergeChecks":[],"userJudgments":[],"resolvableUncertainties":[]}`
	var review map[string]any
	if err := json.Unmarshal([]byte(legacy), &review); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		value          any
		include, valid bool
	}{
		{"old", nil, false, true}, {"specified", []any{map[string]any{"text": "改善", "reason": "早く直す"}}, true, true},
		{"missing reason", []any{map[string]any{"text": "改善"}}, true, false}, {"wrong type", "改善", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.include {
				review["interrupts"] = tc.value
			} else {
				delete(review, "interrupts")
			}
			data, _ := json.Marshal(review)
			if err := Validate(data, true); (err == nil) != tc.valid {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

package testcause

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestFixedPassReportRoundtripAndRejectsMismatchedProof(t *testing.T) {
	m := measured()
	m.Code, m.Output, m.CompletedFiles = 0, "", 1
	original := Create(m)
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(Object){
		"valid":                func(Object) {},
		"missing sha":          func(o Object) { o["sha"] = nil },
		"invalid sha":          func(o Object) { o["sha"] = "invalid" },
		"different proof sha":  func(o Object) { obj(o["passEvidence"])["sha"] = m.BaseSHA },
		"different record sha": func(o Object) { obj(obj(o["passEvidence"])["record"])["sha"] = m.BaseSHA },
		"different conditions": func(o Object) {
			obj(obj(obj(o["passEvidence"])["record"])["execution"])["command"] = obj(o["passEvidence"])["environment"]
		},
		"truncated run":      func(o Object) { obj(obj(o["passEvidence"])["run"])["truncated"] = true },
		"interrupted run":    func(o Object) { obj(obj(o["passEvidence"])["run"])["killed"] = "SIGTERM" },
		"missing conditions": func(o Object) { o["conditionsComplete"] = false },
	} {
		t.Run(name, func(t *testing.T) {
			var recovered Object
			if err := json.Unmarshal(raw, &recovered); err != nil {
				t.Fatal(err)
			}
			mutate(recovered)
			result := Report(Object{"target": recovered}, 24576)
			target := obj(result["target"])
			if name == "valid" {
				if target["complete"] != true || target["sha"] != m.SHA || target["source"] != "fixed-test" {
					t.Fatalf("lost fixed pass: %#v", result)
				}
			} else if target != nil && target["complete"] == true {
				t.Fatalf("accepted invalid proof: %#v", result)
			}
		})
	}
}

func TestReportRevalidatesProofsAndPreservesPublicClassification(t *testing.T) {
	raw, e := os.ReadFile("testdata/report.json")
	if e != nil {
		t.Fatal(e)
	}
	var cases []struct {
		Input    Object
		Expected Object
		Budget   int
	}
	if e = json.Unmarshal(raw, &cases); e != nil {
		t.Fatal(e)
	}
	for index, item := range cases {
		budget := item.Budget
		if budget == 0 {
			budget = 24576
		}
		result := Report(item.Input, budget)
		encoded, _ := json.Marshal(result)
		var got Object
		_ = json.Unmarshal(encoded, &got)
		if !reflect.DeepEqual(got, item.Expected) {
			t.Errorf("case %d got=%s expected=%s", index, encoded, encoding(item.Expected))
		}
	}
}

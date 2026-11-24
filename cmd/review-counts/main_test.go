package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestAggregateCompletionResults(t *testing.T) {
	// Results have the same top-level shape as the worker's completion result.
	input := `[{"status":"merged","reviewCounts":{"full":2,"delta":1,"skipped":1}},{"status":"merged","reviewCounts":{"full":1,"delta":2,"skipped":5}},{"status":"merged","reviewRounds":[{"kind":"full"}]}]`
	var out bytes.Buffer
	if err := run(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var got report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Jobs != 2 || got.Missing != 1 || got.Counts != (counts{Full: 3, Delta: 3, Skipped: 6}) || got.Ratios == nil || got.Ratios.Full != .25 || got.Ratios.Delta != .25 || got.Ratios.Skipped != .5 {
		t.Fatalf("report: %+v", got)
	}
}

func TestAggregateZeroAndInvalidResults(t *testing.T) {
	var out bytes.Buffer
	if err := run(strings.NewReader(`[{"reviewCounts":{"full":0,"delta":0,"skipped":0}},{}]`), &out); err != nil {
		t.Fatal(err)
	}
	var got report
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Jobs != 1 || got.Missing != 1 || got.Ratios != nil {
		t.Fatalf("report: %+v", got)
	}
	for _, input := range []string{`null`, `[null]`, `[{"reviewCounts":{"full":-1}}]`, `[] []`} {
		if err := run(strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

package execution

import (
	"encoding/json"
	"reflect"
)

// normalizeReviewInterrupts uses the original item classifications before findings move.
func normalizeReviewInterrupts(review Review, raw json.RawMessage) (Review, json.RawMessage) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil || object["interrupts"] == nil {
		return review, raw
	}
	counts := map[string]int{}
	blocked := map[string]bool{}
	for i, text := range review.Findings {
		counts[text]++
		if i >= len(review.Kinds) || i >= len(review.Severities) || review.Severities[i] != "minor" || (review.Kinds[i] != "regression" && review.Kinds[i] != "acceptance" && review.Kinds[i] != "other") {
			blocked[text] = true
		}
	}
	for _, list := range [][]string{review.Improvements, review.ResolvableUncertainties} {
		for _, text := range list {
			counts[text]++
		}
	}
	for _, list := range [][]string{review.RecordOnly, review.UserJudgments} {
		for _, text := range list {
			blocked[text] = true
		}
	}
	requested := map[string]int{}
	for _, item := range review.Interrupts {
		requested[item.Text]++
	}
	kept := make([]reviewInterrupt, 0, len(review.Interrupts))
	for _, item := range review.Interrupts {
		reason := normalizedInterruptReason(item.Reason)
		if item.Text != "" && counts[item.Text] == 1 && !blocked[item.Text] && requested[item.Text] == 1 && reason != "" {
			kept = append(kept, reviewInterrupt{Text: item.Text, Reason: reason})
		}
	}
	if reflect.DeepEqual(review.Interrupts, kept) {
		return review, raw
	}
	review.Interrupts = kept
	object["interrupts"] = encode(kept)
	return review, encode(object)
}

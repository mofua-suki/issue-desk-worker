package execution

import (
	"encoding/json"
	"issue-desk-worker/internal/gitwork"
	"strings"

	"issue-desk-worker/internal/journal"
)

// removeRecordedFinalApproval preserves the provider's complete review while
// removing only the judgment covered by the verified publication head.
func removeRecordedFinalApproval(review Review, raw json.RawMessage, repeated []string) (Review, json.RawMessage) {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return review, raw
	}
	var kept, removed []string
	for _, item := range review.UserJudgments {
		if strings.Contains(item, "最終承認") {
			removed = append(removed, item)
		} else {
			kept = append(kept, item)
		}
	}
	if len(removed) == 0 {
		return review, raw
	}
	var checks []string
	if v, exists := value["postMergeChecks"]; exists && json.Unmarshal(v, &checks) != nil {
		return review, raw
	}
	value["userJudgments"] = encode(kept)
	value["postMergeChecks"] = encode(append(checks, removed...))
	review.UserJudgments = kept
	if review.Decision == "hold" && len(repeated) == 0 {
		candidate := review
		candidate.Decision = "approve"
		if candidate.Approved() {
			value["decision"] = encode("approve")
		}
	}
	return normalizeReview(encode(value))
}

func (r Review) Approved() bool {
	if r.ReviewComparisonBlocked || r.Breaking || r.TooLarge || len(r.Uncertainties) > 0 || len(r.UserJudgments) > 0 {
		return false
	}
	if len(r.Findings) == 0 {
		return r.Decision == "approve" || len(r.ResolvableUncertainties) > 0
	}
	if len(r.Kinds) != len(r.Findings) || len(r.Severities) != len(r.Findings) {
		return false
	}
	for i, kind := range r.Kinds {
		if r.Severities[i] != "minor" {
			return false
		}
		switch kind {
		case "regression", "acceptance", "other":
			// These kinds may be deferred only when explicitly minor.
		default:
			return false
		}
	}
	return true
}
func normalizeReview(raw json.RawMessage) (Review, json.RawMessage) {
	var review Review
	_ = json.Unmarshal(raw, &review)
	review, raw = normalizeReviewInterrupts(review, raw)
	if !review.Approved() || (len(review.Findings) == 0 && len(review.ResolvableUncertainties) == 0) {
		return review, raw
	}
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	improvements, _ := value["improvements"].([]any)
	for _, finding := range append(append([]string{}, review.Findings...), review.ResolvableUncertainties...) {
		improvements = append(improvements, finding)
	}
	value["improvements"] = improvements
	// Worker-owned metadata survives clearing the provider's findingSpecs.
	value["improvementSpecs"] = reviewProposalItems(review)
	value["decision"] = "approve"
	value["findings"] = []any{}
	value["findingKinds"] = []any{}
	value["findingSeverities"] = []any{}
	value["findingSpecs"] = []any{}
	value["resolvableUncertainties"] = []any{}
	data := encode(value)
	_ = json.Unmarshal(data, &review)
	return review, data
}

// comparableReview is deliberately stricter than legacy repetition detection.
// An incomplete/omitted report is not evidence that a must finding is new.
func comparableReview(raw json.RawMessage) (Review, bool) {
	var review Review
	var object map[string]json.RawMessage
	if !usableCarried(raw) || json.Unmarshal(raw, &review) != nil || json.Unmarshal(raw, &object) != nil {
		return review, false
	}
	if review.ReviewComparisonBlocked || review.Decision != "approve" && review.Decision != "hold" {
		return review, false
	}
	if review.ReviewedHead != "" && !gitwork.ValidSHA(review.ReviewedHead) || review.ReviewedGeneration != nil && *review.ReviewedGeneration < 0 || review.ReviewedEpoch < 0 {
		return review, false
	}
	for _, key := range []string{"findings", "findingKinds", "findingSeverities"} {
		if !hasJSON(object[key]) {
			return review, false
		}
	}
	if len(review.Findings) != len(review.Kinds) || len(review.Findings) != len(review.Severities) || (len(review.FindingSpecs) > 0 && len(review.FindingSpecs) != len(review.Findings)) {
		return review, false
	}
	for i, finding := range review.Findings {
		if reviewTextKey(finding) == "" || (review.Severities[i] != "must" && review.Severities[i] != "minor") {
			return review, false
		}
		switch review.Kinds[i] {
		case "acceptance", "other", "security", "regression", "data", "blocking":
		default:
			return review, false
		}
	}
	return review, true
}

func reviewTextKey(text string) string { return strings.Join(strings.Fields(text), " ") }

func heavyFindingKeys(r Review, i int) []string {
	if i < len(r.Severities) && r.Severities[i] == "minor" {
		return nil
	}
	var keys []string
	if text := reviewTextKey(r.Findings[i]); text != "" {
		keys = append(keys, text)
	}
	if i < len(r.FindingSpecs) {
		if problem := reviewTextKey(r.FindingSpecs[i].Problem); len([]rune(problem)) >= 10 {
			keys = append(keys, problem)
		}
	}
	return keys
}

// decideReview compares untouched responses before normalizing or replacing r.Review.
// Carryover has already selected the newest evidence.
func decideReview(r *journal.Record, raw json.RawMessage, _ ...bool) (Review, json.RawMessage, []string) {
	previous := firstJSON(r.Review, r.PreviousReview)
	prior, comparable := comparableReview(previous)
	links := validPreviousFindingLinks(prior, currentReview(raw), comparable)
	current, valid := comparableReview(raw)
	var repeated []string
	oldHead := prior.ReviewedHead
	if oldHead == "" && hasJSON(r.Review) {
		oldHead = r.ReviewHead
	}
	if oldHead != "" && oldHead != r.Head {
		if comparable && valid {
			repeated = repeatedHeavyFindings(previous, current, links)
		} else {
			repeated = repeatedHeavyFindings(previous, current)
		}
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || value == nil {
		review, saved := normalizeReview(raw)
		return review, saved, repeated
	}
	// Provider output cannot set Worker-owned delivery metadata.
	delete(value, "reviewArchive")
	delete(value, "deferredReviewItems")
	delete(value, "deferredReviewDeliveries")
	delete(value, "reviewComparisonBlocked")
	delete(value, "reviewedHead")
	delete(value, "reviewedGeneration")
	delete(value, "reviewedEpoch")
	deferred, deliveries := retainedReviewData(r.PreviousReview, r.Review)
	value["reviewedHead"] = encode(r.Head)
	value["reviewedGeneration"] = encode(r.Generation)
	value["reviewedEpoch"] = encode(r.Epoch)
	value["deferredReviewItems"] = encode(deferred)
	value["deferredReviewDeliveries"] = encode(deliveries)
	review, saved := normalizeReview(encode(value))
	review, saved = attachDeferredReviewItems(review, saved, deferred)
	return review, saved, repeated
}

// An explicit actionable classification wins a conflict; missing metadata must
// not erase an explicit classification during carry and deduplication.
func mergeCheckClassification(a, b *bool) *bool {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if !*a {
		return a
	}
	return b
}

func mergeDeferredReviewItems(items, added []followupProposalItem) []followupProposalItem {
	seen := map[string]int{}
	for i, item := range items {
		seen[reviewTextKey(item.Problem)] = i + 1
	}
	for _, item := range added {
		key := reviewTextKey(item.Problem)
		if key == "" {
			continue
		}
		if index := seen[key]; index != 0 {
			old := &items[index-1]
			old.ExternalCheckOnly = mergeCheckClassification(old.ExternalCheckOnly, item.ExternalCheckOnly)
			if old.InterruptReason == "" && old.Problem == item.Problem {
				old.InterruptReason = item.InterruptReason
			}
		} else {
			items = append(items, item)
			seen[key] = len(items)
		}
	}
	return items
}

// Held reviews retain only metadata; approval connects it to the existing
// improvement/spec normalization and delivery path, exactly once per body.
func attachDeferredReviewItems(review Review, raw json.RawMessage, items []followupProposalItem) (Review, json.RawMessage) {
	if len(items) == 0 {
		return review, raw
	}
	var value map[string]json.RawMessage
	_ = json.Unmarshal(raw, &value)
	items = append([]followupProposalItem(nil), items...)
	for i := range items {
		for _, proposal := range reviewProposalItems(review) {
			if items[i].Problem == proposal.Problem && proposal.InterruptReason != "" {
				items[i].InterruptReason = proposal.InterruptReason
			}
		}
	}
	value["deferredReviewItems"] = encode(items)
	forced := map[string]bool{}
	for _, item := range items {
		forced[reviewTextKey(item.Problem)] = true
	}
	var recordOnly []string
	for _, text := range review.RecordOnly {
		if !forced[reviewTextKey(text)] {
			recordOnly = append(recordOnly, text)
		}
	}
	value["recordOnly"] = encode(recordOnly)
	if review.Approved() {
		proposals := reviewProposalItems(review)
		for _, item := range items {
			found := false
			for i := range proposals {
				if reviewTextKey(proposals[i].Problem) == reviewTextKey(item.Problem) {
					item.ExternalCheckOnly = mergeCheckClassification(item.ExternalCheckOnly, proposals[i].ExternalCheckOnly)
					if item.Problem == proposals[i].Problem && proposals[i].InterruptReason != "" {
						item.InterruptReason = proposals[i].InterruptReason
					}
					proposals[i] = item
					found = true
				}
			}
			if !found {
				proposals = append(proposals, item)
			}
		}
		// Deduplicate only deferred items; unrelated improvements keep their order.
		var improvements []string
		var specs []followupProposalItem
		seen := map[string]bool{}
		for _, item := range proposals {
			key := reviewTextKey(item.Problem)
			if forced[key] && seen[key] {
				continue
			}
			seen[key] = true
			improvements = append(improvements, item.Problem)
			specs = append(specs, item)
		}
		value["improvements"], value["improvementSpecs"] = encode(improvements), encode(specs)
	}
	data := encode(value)
	_ = json.Unmarshal(data, &review)
	return review, data
}

// rememberReview runs before any review invalidation. PreviousReview already
// survives retry, generation changes, interrupted runs and intake result carry.
func rememberReview(r *journal.Record) {
	if !hasJSON(r.Review) {
		return
	}
	r.Review = stampReviewEvidence(r.Review, r.Generation, r.Epoch, r.ReviewHead)
	var review Review
	var value map[string]json.RawMessage
	if json.Unmarshal(r.Review, &review) != nil || json.Unmarshal(r.Review, &value) != nil || value == nil {
		r.PreviousReview = r.Review
		return
	}
	r.PreviousReview = mergeRetainedReview(r.Review, r.PreviousReview)
}

// Preserve metadata already carried by a review when invalidating it or
// importing a legacy report with only a checkpoint generation.
func stampReviewEvidence(raw json.RawMessage, generation int, epoch int64, head string) json.RawMessage {
	var review Review
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &review) != nil || json.Unmarshal(raw, &value) != nil || value == nil {
		return raw
	}
	if review.ReviewedGeneration == nil {
		value["reviewedGeneration"] = encode(generation)
	}
	if review.ReviewedEpoch == 0 && epoch > 0 {
		value["reviewedEpoch"] = encode(epoch)
	}
	if review.ReviewedHead == "" && head != "" {
		value["reviewedHead"] = encode(head)
	}
	return encode(value)
}

// Compare generation first and lease epoch second. Within one lease, a locally
// produced review is newer than the descriptor received at the lease's start.
// Older Workers lack these fields: their checkpoint generation still separates
// a carried result from a review produced locally in the current generation.
func seedPreviousReview(r *journal.Record, d Descriptor, candidates ...json.RawMessage) {
	var incoming json.RawMessage
	for _, candidate := range candidates {
		if hasJSON(candidate) {
			incoming, _ = expandCarriedReview(candidate)
			break
		}
	}
	if !hasJSON(incoming) {
		return
	}
	if items, _ := retainedReviewData(incoming); len(items) > 0 {
		retainSentReviewDeliveries(r)
	}
	local := firstJSON(r.Review, r.PreviousReview)
	var before, delivered Review
	_ = json.Unmarshal(local, &before)
	_ = json.Unmarshal(incoming, &delivered)
	// Validate both reports before their ordering metadata can affect selection.
	_, localValid := comparableReview(local)
	_, incomingValid := comparableReview(incoming)
	checkpoint := field[startedExecution](d.View, "workerExecution")
	incomingGeneration := max(0, d.Generation-1)
	if checkpoint.Generation != nil {
		incomingGeneration = *checkpoint.Generation
	}
	if delivered.ReviewedGeneration != nil {
		incomingGeneration = *delivered.ReviewedGeneration
	}
	blocked := !incomingValid || hasJSON(local) && !localValid || incomingGeneration < 0
	preferLocal := hasJSON(local) && (localValid || !incomingValid)
	if !blocked {
		localGeneration := r.Generation
		if before.ReviewedGeneration != nil {
			localGeneration = *before.ReviewedGeneration
		}
		preferLocal = localGeneration > incomingGeneration
		if localGeneration == incomingGeneration {
			if before.ReviewedEpoch > 0 && delivered.ReviewedEpoch > 0 {
				preferLocal = before.ReviewedEpoch >= delivered.ReviewedEpoch
			} else {
				preferLocal = localGeneration == d.Generation
			}
		}
	}
	chosen := incoming
	if hasJSON(local) && preferLocal {
		chosen = local
	} else {
		if _, comparable := comparableReview(chosen); comparable {
			chosen = stampReviewEvidence(chosen, incomingGeneration, delivered.ReviewedEpoch, "")
		}
		r.Review = nil
		r.ReviewBaseline = nil
		r.CarriedHeldReview = nil
		r.ReviewSkipped = false
		if r.Stage == "publish" || r.Stage == "merge" {
			r.Stage = "review"
		}
	}
	chosen = mergeRetainedReview(chosen, r.PreviousReview, local, incoming)
	if _, chosenValid := comparableReview(chosen); blocked && chosenValid {
		var value map[string]json.RawMessage
		if json.Unmarshal(chosen, &value) == nil && value != nil {
			value["reviewComparisonBlocked"] = encode(true)
			chosen = encode(value)
			r.ReviewBaseline = nil
			r.CarriedHeldReview = nil
			r.ReviewSkipped = false
			if r.Stage == "publish" || r.Stage == "merge" {
				r.Stage = "review"
			}
		}
	}
	if hasJSON(r.Review) {
		replaceRetainedReview(r, chosen)
	} else {
		r.PreviousReview = chosen
	}
	restoreDeferredDeliveryResults(r, chosen)
}

// holdSummary puts the actionable reason before the potentially long review summary.
func (r Review) holdSummary(repeated ...string) string {
	var reasons []string
	if len(repeated) > 0 {
		reasons = append(reasons, "修正後も残る指摘: "+strings.Join(repeated, "、"))
	}
	for _, item := range append(append([]string{}, r.UserJudgments...), r.Uncertainties...) {
		reasons = append(reasons, "利用者の判断: "+item)
	}
	if r.Breaking {
		detail := strings.Join(r.Findings, "、")
		if detail == "" {
			detail = r.Summary
		}
		reasons = append(reasons, "破壊的変更: "+detail)
	}
	if len(reasons) == 0 {
		reasons = append(reasons, "レビュー保留: "+r.Summary)
	}
	return strings.Join(reasons, "。") + "。" + r.Summary
}

// repeatedHeavyFindings matches heavy findings by normalized finding text or
// a structured problem of at least 10 characters, independent of review kind.
// Structured problems allow the surrounding finding wording to change.
func repeatedHeavyFindings(previous json.RawMessage, current Review, links ...map[int]bool) []string {
	var prior Review
	if json.Unmarshal(previous, &prior) != nil {
		return nil
	}
	priorKeys := make([][]string, len(prior.Findings))
	for i := range prior.Findings {
		priorKeys[i] = heavyFindingKeys(prior, i)
	}
	var repeated []string
	for i, finding := range current.Findings {
		currentKeys := heavyFindingKeys(current, i)
		matched := false
		for j, oldKeys := range priorKeys {
			for _, a := range currentKeys {
				for _, b := range oldKeys {
					if a == b {
						matched = true
						break
					}
				}
				if matched {
					break
				}
			}
			if matched {
				priorKeys[j] = nil
				break
			}
		}
		if matched || len(links) > 0 && links[0][i] {
			repeated = append(repeated, finding)
		}
	}
	return repeated
}

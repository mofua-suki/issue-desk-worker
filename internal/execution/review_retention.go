package execution

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"issue-desk-worker/internal/journal"
)

// reviewPromptData removes delivery bookkeeping from an input-only copy.
// Preserve malformed reports as text so encoding the enclosing prompt cannot
// silently discard their findings along with the invalid JSON.
func reviewPromptData(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		if json.Valid(raw) {
			return append(json.RawMessage(nil), raw...)
		}
		return encode(string(raw))
	}
	if _, ok := value["deferredReviewDeliveries"]; !ok {
		return append(json.RawMessage(nil), raw...)
	}
	delete(value, "deferredReviewDeliveries")
	return encode(value)
}

// Each attempted wire item keeps its original proposal identity. Tracking parts
// separately lets unattempted parts of a long finding survive an interruption.
type deferredReviewDelivery struct {
	Author   string                   `json:"author"`
	Part     int                      `json:"part"`
	Source   string                   `json:"source"`
	Item     followupProposalItem     `json:"item,omitzero"`
	ItemHash string                   `json:"itemHash,omitempty"`
	Proposal journal.FollowupProposal `json:"proposal"`
}

// Older saved reviews contain the wire text; new records need only its digest.
// The full proposal detail remains in deferredReviewItems/improvementSpecs.
func deliveryItemHash(delivery deferredReviewDelivery) string {
	if delivery.ItemHash != "" {
		return delivery.ItemHash
	}
	return digestReview(string(encode(delivery.Item)))
}

func mergeDeferredDeliveries(items, added []deferredReviewDelivery) []deferredReviewDelivery {
	for _, item := range added {
		found := false
		for i, old := range items {
			if old.Source == item.Source && old.Part == item.Part && deliveryItemHash(old) == deliveryItemHash(item) && (old.Source != "" || old.Proposal.ID == item.Proposal.ID) {
				// A sent result cannot be replaced by an older unknown response.
				if old.Proposal.Status != "sent" {
					items[i] = item
				}
				found = true
				break
			}
		}
		if !found {
			items = append(items, item)
		}
	}
	return items
}

func retainedReviewData(reports ...json.RawMessage) ([]followupProposalItem, []deferredReviewDelivery) {
	var items []followupProposalItem
	var deliveries []deferredReviewDelivery
	for _, raw := range reports {
		var review Review
		if json.Unmarshal(raw, &review) == nil {
			items = mergeDeferredReviewItems(items, review.DeferredReviewItems)
			deliveries = mergeDeferredDeliveries(deliveries, review.DeferredReviewDeliveries)
		}
	}
	return items, deliveries
}

// Selection of comparison evidence must not select away retained proposals.
// Approval also needs the merged details immediately, without another AI call.
func mergeRetainedReview(chosen json.RawMessage, reports ...json.RawMessage) json.RawMessage {
	items, deliveries := retainedReviewData(append(reports, chosen)...)
	if len(items) == 0 && len(deliveries) == 0 {
		return chosen
	}
	var review Review
	var value map[string]json.RawMessage
	if json.Unmarshal(chosen, &review) != nil || json.Unmarshal(chosen, &value) != nil || value == nil {
		return chosen
	}
	value["deferredReviewItems"] = encode(items)
	value["deferredReviewDeliveries"] = encode(deliveries)
	raw := encode(value)
	_ = json.Unmarshal(raw, &review)
	_, raw = attachDeferredReviewItems(review, raw, items)
	return raw
}

// Metadata changes preserve an otherwise valid baseline's exact review binding.
// A mismatched baseline remains invalid; this never repairs untrusted hashes.
func replaceRetainedReview(r *journal.Record, raw json.RawMessage) {
	if b := r.ReviewBaseline; b != nil && b.ReviewHash == digestReviewJSON(r.Review) && b.ReviewHash == digestReviewJSON(b.Review) && b.ReviewHash != "" {
		b.Review = append(json.RawMessage(nil), raw...)
		b.ReviewHash = digestReviewJSON(raw)
	}
	r.Review = raw
}

func restoreRetainedBaseline(r *journal.Record) {
	raw := mergeRetainedReview(r.ReviewBaseline.Review, r.PreviousReview, r.Review)
	r.ReviewBaseline.Review = append(json.RawMessage(nil), raw...)
	r.ReviewBaseline.ReviewHash = digestReviewJSON(raw)
	r.Review = raw
	restoreDeferredDeliveryResults(r, raw)
}

func restoreDeferredDeliveryResults(r *journal.Record, raw json.RawMessage) {
	_, deliveries := retainedReviewData(raw)
	for _, delivery := range deliveries {
		updateFollowupResult(r, delivery.Proposal)
	}
}

func updateFollowupResult(r *journal.Record, state journal.FollowupProposal) {
	for i := range r.FollowupProposals {
		if r.FollowupProposals[i].ID == state.ID {
			if r.FollowupProposals[i].Status != "sent" {
				r.FollowupProposals[i] = state
			}
			return
		}
	}
	r.FollowupProposals = append(r.FollowupProposals, state)
}

// Only parts of retained findings use this cross-generation history. Other
// improvements retain the existing per-generation/head delivery contract.
type deferredReviewSource struct {
	Key  string
	Part int
}

func deferredProposalWireItems(r *journal.Record, review Review, selected []followupProposalItem) ([]followupProposalItem, []deferredReviewSource, int) {
	batchBase := 0
	for _, delivery := range review.DeferredReviewDeliveries {
		if delivery.Proposal.Generation == r.Generation && delivery.Proposal.Head == r.Head {
			parts := strings.Split(delivery.Proposal.ID, "-")
			batch, err := strconv.Atoi(parts[len(parts)-1])
			if err == nil {
				batchBase = max(batchBase, batch+1)
			}
		}
	}
	deferred := map[string]bool{}
	for _, item := range review.DeferredReviewItems {
		deferred[reviewTextKey(item.Problem)] = true
	}
	var items []followupProposalItem
	var sources []deferredReviewSource
	for _, item := range selected {
		source := reviewTextKey(item.Problem)
		if !deferred[source] {
			source = ""
		} else {
			source = digestReview(source)
		}
		for partIndex, part := range proposalWireItems([]followupProposalItem{item}) {
			attempted := false
			for _, delivery := range review.DeferredReviewDeliveries {
				sameRound := delivery.Proposal.Generation == r.Generation && delivery.Proposal.Head == r.Head
				if delivery.Source == source && delivery.Part == partIndex && deliveryItemHash(delivery) == digestReview(string(encode(wireProposalItem(part)))) && (source != "" || sameRound) {
					attempted = true
					break
				}
			}
			if !attempted {
				items, sources = append(items, part), append(sources, deferredReviewSource{Key: source, Part: partIndex})
			}
		}
	}
	return items, sources, batchBase
}

func saveDeferredDelivery(r *journal.Record, items []followupProposalItem, sources []deferredReviewSource, state journal.FollowupProposal, author string, refused bool) {
	var review Review
	var value map[string]json.RawMessage
	_ = json.Unmarshal(r.Review, &review)
	_ = json.Unmarshal(r.Review, &value)
	var retained []deferredReviewDelivery
	for _, delivery := range review.DeferredReviewDeliveries {
		if delivery.Proposal.ID != state.ID {
			retained = append(retained, delivery)
		}
	}
	if !refused {
		for i, item := range items {
			retained = append(retained, deferredReviewDelivery{Author: author, Source: sources[i].Key, Part: sources[i].Part, ItemHash: digestReview(string(encode(wireProposalItem(item)))), Proposal: state})
		}
	}
	value["deferredReviewDeliveries"] = encode(retained)
	replaceRetainedReview(r, encode(value))
	updateFollowupResult(r, state)
}

// Unknown deliveries may be recovered by the original author's marker, including
// on a PC without the old outbox. This path never POSTs or changes proposal IDs.
func (e *Executor) reconcileDeferredDeliveries(ctx context.Context, r *journal.Record) error {
	var review Review
	_ = json.Unmarshal(r.Review, &review)
	seen := map[string]bool{}
	changed := false
	for _, delivery := range review.DeferredReviewDeliveries {
		id := delivery.Proposal.ID
		if delivery.Proposal.Status == "sent" || seen[id] {
			continue
		}
		seen[id] = true
		n := notification{Kind: "followup", Repo: r.Repo, Target: r.Issue, Head: delivery.Proposal.Head, Author: delivery.Author, Marker: "<!-- issue-desk-followup:" + id + " -->"}
		found, err := e.noticeFound(ctx, n)
		if err != nil || !found {
			continue // Read failures remain delivery data, not merge gates.
		}
		for i := range review.DeferredReviewDeliveries {
			if review.DeferredReviewDeliveries[i].Proposal.ID == id {
				review.DeferredReviewDeliveries[i].Proposal.Status = "sent"
				review.DeferredReviewDeliveries[i].Proposal.DeliveryOutcome = "confirmed"
				review.DeferredReviewDeliveries[i].Proposal.Error = ""
			}
		}
		changed = true
	}
	if changed {
		var value map[string]json.RawMessage
		_ = json.Unmarshal(r.Review, &value)
		value["deferredReviewDeliveries"] = encode(review.DeferredReviewDeliveries)
		replaceRetainedReview(r, encode(value))
		restoreDeferredDeliveryResults(r, r.Review)
		return e.Store.Save(r)
	}
	return ctx.Err()
}

// When adding carried findings to an already published approval, preserve the
// identity of its sent batches before changing the delivery material. This also
// leaves room for a new batch at the same head without reposting its old items.
func retainSentReviewDeliveries(r *journal.Record) {
	var review Review
	if json.Unmarshal(r.Review, &review) != nil || !review.Approved() || len(review.DeferredReviewDeliveries) > 0 {
		return
	}
	excluded := map[string]bool{}
	for _, text := range review.RecordOnly {
		excluded[reviewTextKey(text)] = true
	}
	var selected []followupProposalItem
	for _, item := range reviewProposalItemsForDelivery(review, true) {
		if !excluded[reviewTextKey(item.Problem)] {
			selected = append(selected, item)
		}
	}
	items, sources, _ := deferredProposalWireItems(r, review, selected)
	for offset := 0; offset < len(items); offset += 5 {
		id := followupProposalID(r, offset/5)
		for _, state := range r.FollowupProposals {
			if state.ID == id && (state.Status == "sent" || state.Status == "failed" && (state.DeliveryOutcome == "unknown" || state.DeliveryOutcome == "" && strings.Contains(state.Error, "未確認"))) {
				end := min(offset+5, len(items))
				saveDeferredDelivery(r, items[offset:end], sources[offset:end], state, "", false)
				break
			}
		}
	}
}

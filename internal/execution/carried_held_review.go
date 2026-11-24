package execution

import (
	"bytes"
	"encoding/json"
	"io"
	"issue-desk-worker/internal/gitwork"
	"regexp"

	"issue-desk-worker/internal/journal"
)

var carriedDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

// carriedReviewDigest identifies a review by its JSON value. The intake decodes
// reports and stores them as jsonb, so key order, escaping and number spelling
// of the delivered review differ from the bytes this Worker sent. The local
// baseline keeps its byte digest; only the carried state uses this one.
func carriedReviewDigest(raw json.RawMessage) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return ""
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ""
	}
	var normalize func(any) any
	normalize = func(v any) any {
		switch v := v.(type) {
		case json.Number:
			if i, err := v.Int64(); err == nil {
				return i
			}
			if f, err := v.Float64(); err == nil {
				return f
			}
			return v.String()
		case []any:
			for i := range v {
				v[i] = normalize(v[i])
			}
		case map[string]any:
			for key, field := range v {
				v[key] = normalize(field)
			}
		}
		return v
	}
	// Maps are encoded in sorted key order.
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(normalize(value)) != nil {
		return ""
	}
	return digestReview(out.String())
}

// carriedHeldValid checks the complete identity, including the Worker's review
// stamp. A truncated result or an older format is not evidence of a hold.
func carriedHeldValid(c *journal.CarriedHeldReview, review json.RawMessage) bool {
	if c == nil || c.Version != 1 || c.ID < 1 || c.Repo == "" || c.Issue < 1 || c.Generation < 0 || c.Epoch < 1 || c.Decision != "held" || !gitwork.ValidSHA(c.Head) || !carriedDigest.MatchString(c.DiffHash) || !carriedDigest.MatchString(c.RequestHash) || !carriedDigest.MatchString(c.ReviewHash) || carriedReviewDigest(review) != c.ReviewHash {
		return false
	}
	parsed, ok := comparableReview(review)
	return ok && !parsed.Approved() && parsed.ReviewedGeneration != nil && *parsed.ReviewedGeneration == c.Generation && parsed.ReviewedEpoch == c.Epoch && parsed.ReviewedHead == c.Head
}

// carriedHeldMatches is the patch-free held evidence boundary for callers
// which have independently verified the current workspace and test evidence.
func carriedHeldMatches(r *journal.Record, d Descriptor, requestHash, diff string) bool {
	c := r.CarriedHeldReview
	if !carriedHeldValid(c, cReview(c)) || c.ID != d.ID || c.Repo != d.Repo || c.Issue != d.Issue || c.Generation != d.Generation || c.RequestHash != requestHash || c.Epoch >= r.Epoch || retryBaseApplied(d, r) || retryBaseNeeded(d, r) {
		return false
	}
	if carriedReviewDigest(firstJSON(r.Review, r.PreviousReview)) != c.ReviewHash {
		return false
	}
	hash, ok := reviewDiffHash(diff)
	return ok && hash == c.DiffHash
}

func cReview(c *journal.CarriedHeldReview) json.RawMessage {
	if c == nil {
		return nil
	}
	return c.Review
}

// The wire format excludes the review and the patch. The existing result carries
// the review; its digest binds these independently stored pieces together.
func (e *Executor) addCarriedHeldReview(view map[string]any, r *journal.Record) {
	var c *journal.CarriedHeldReview
	if b := r.ReviewBaseline; b != nil {
		if _, ok := loadReviewBaseline(e.Config.RuntimeDir, r); ok && b.Decision == "held" && b.Generation == r.Generation && digestReviewJSON(r.Review) == b.ReviewHash {
			var review Review
			_ = json.Unmarshal(b.Review, &review)
			candidate := &journal.CarriedHeldReview{Version: 1, ID: r.ID, Repo: r.Repo, Issue: r.Issue, Generation: b.Generation, Epoch: review.ReviewedEpoch, Decision: b.Decision, Head: b.Head, DiffHash: b.Hash, RequestHash: b.RequestHash, ReviewHash: carriedReviewDigest(b.Review), Review: b.Review}
			if carriedHeldValid(candidate, b.Review) {
				c = candidate
			}
		}
	} else if r.CarriedHeldReview != nil && r.CarriedHeldReview.Generation == r.Generation && carriedHeldValid(r.CarriedHeldReview, r.CarriedHeldReview.Review) && (carriedReviewDigest(r.Review) == r.CarriedHeldReview.ReviewHash || carriedReviewDigest(r.PreviousReview) == r.CarriedHeldReview.ReviewHash) {
		c = r.CarriedHeldReview
	}
	if c == nil {
		return
	}
	// Explicit wire shape prevents accidentally shipping the patch or review.
	view["carriedHeldReview"] = struct {
		Version     int    `json:"version"`
		ID          int64  `json:"id"`
		Repo        string `json:"repo"`
		Issue       int64  `json:"issue"`
		Generation  int    `json:"generation"`
		Epoch       int64  `json:"epoch"`
		Decision    string `json:"decision"`
		Head        string `json:"head"`
		DiffHash    string `json:"diffHash"`
		RequestHash string `json:"requestHash"`
		ReviewHash  string `json:"reviewHash"`
	}{c.Version, c.ID, c.Repo, c.Issue, c.Generation, c.Epoch, c.Decision, c.Head, c.DiffHash, c.RequestHash, c.ReviewHash}
}

// Only a fresh journal may import a checkpoint. Existing journals, including
// retry tombstones and newer decisions, always win over a delivered snapshot.
func (e *Executor) restoreCarriedHeldReview(r *journal.Record, d Descriptor, requestHash string, fresh bool) {
	if r.CarriedHeldReview != nil && (r.CarriedHeldReview.Generation != d.Generation || r.CarriedHeldReview.RequestHash != requestHash || retryBaseApplied(d, r)) {
		r.CarriedHeldReview = nil
	}
	if !fresh || retryBaseNeeded(d, r) || retryBaseApplied(d, r) {
		return
	}
	var checkpoint struct {
		Held json.RawMessage `json:"carriedHeldReview"`
	}
	if json.Unmarshal(d.View["workerExecution"], &checkpoint) != nil {
		return
	}
	var c journal.CarriedHeldReview
	if json.Unmarshal(checkpoint.Held, &c) != nil {
		return
	}
	var fields struct {
		Generation *int `json:"generation"`
	}
	if json.Unmarshal(checkpoint.Held, &fields) != nil || fields.Generation == nil {
		return
	}
	var result struct {
		Review   json.RawMessage `json:"review"`
		Previous json.RawMessage `json:"previousReview"`
	}
	if json.Unmarshal(d.Result, &result) != nil {
		return
	}
	review, _ := expandCarriedReview(firstJSON(result.Review, result.Previous))
	if c.ID != d.ID || c.Repo != d.Repo || c.Issue != d.Issue || c.Generation != d.Generation || c.RequestHash != requestHash || c.Epoch >= r.Epoch || !carriedHeldValid(&c, review) {
		return
	}
	// Do not let the review selected by the existing newness/retention logic be
	// replaced by an older or incomparable delivered review.
	selected := firstJSON(r.Review, r.PreviousReview)
	if carriedReviewDigest(selected) != c.ReviewHash {
		return
	}
	c.Review = append(json.RawMessage(nil), review...)
	r.CarriedHeldReview = &c
}

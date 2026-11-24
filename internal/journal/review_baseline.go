package journal

import "encoding/json"

// ReviewBaseline identifies the reviewed request and decision. Hash and PatchHash
// are empty when the reviewed patch cannot be compared.
type ReviewBaseline struct {
	RequestHash string          `json:"requestHash"`
	Hash        string          `json:"hash"`
	PatchHash   string          `json:"patchHash"`
	ReviewHash  string          `json:"reviewHash"`
	Decision    string          `json:"decision"`
	Head        string          `json:"head"`
	Generation  int             `json:"generation"`
	Review      json.RawMessage `json:"review"`
}

// CarriedHeldReview is Worker-owned evidence; the patch itself remains local.
type CarriedHeldReview struct {
	Version     int             `json:"version"`
	ID          int64           `json:"id"`
	Repo        string          `json:"repo"`
	Issue       int64           `json:"issue"`
	Generation  int             `json:"generation"`
	Epoch       int64           `json:"epoch"`
	Decision    string          `json:"decision"`
	Head        string          `json:"head"`
	DiffHash    string          `json:"diffHash"`
	RequestHash string          `json:"requestHash"`
	ReviewHash  string          `json:"reviewHash"`
	Review      json.RawMessage `json:"review,omitempty"`
}

type ReviewRound struct {
	Kind string `json:"kind"`
	Base string `json:"base"`
	Head string `json:"head"`
}

// ReviewCounts counts all known rounds independently of the retained history.
type ReviewCounts struct {
	Full    int `json:"full"`
	Delta   int `json:"delta"`
	Skipped int `json:"skipped"`
}

func (c *ReviewCounts) add(kind string) {
	switch kind {
	case "full":
		c.Full++
	case "delta":
		c.Delta++
	case "skipped":
		c.Skipped++
	}
}

// EnsureReviewCounts initializes legacy records from the history still available.
// Rounds discarded by older versions cannot be recovered.
func (r *Record) EnsureReviewCounts() {
	if r.ReviewCounts == nil {
		r.ReviewCounts = &ReviewCounts{}
		for _, round := range r.ReviewRounds {
			r.ReviewCounts.add(round.Kind)
		}
	}
}

// MaxReviewRounds bounds both persisted history and the result sent to intake.
const MaxReviewRounds = 20

// LimitReviewRounds retains the most recent rounds, including for old records.
func LimitReviewRounds(rounds []ReviewRound) []ReviewRound {
	if len(rounds) > MaxReviewRounds {
		return append([]ReviewRound(nil), rounds[len(rounds)-MaxReviewRounds:]...)
	}
	return rounds
}

func (r *Record) AddReviewRound(round ReviewRound) {
	r.EnsureReviewCounts()
	r.ReviewCounts.add(round.Kind)
	r.ReviewRounds = LimitReviewRounds(append(r.ReviewRounds, round))
}

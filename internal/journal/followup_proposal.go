package journal

// FollowupProposal records delivery separately from acceptance transfers. An
// unsuccessful delivery never changes the review or merge decision.
type FollowupProposal struct {
	ID              string `json:"id"`
	Generation      int    `json:"generation"`
	Head            string `json:"head"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
	DeliveryOutcome string `json:"deliveryOutcome,omitempty"`
}

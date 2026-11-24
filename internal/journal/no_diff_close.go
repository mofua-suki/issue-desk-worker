package journal

// NoDiffClose records the evidence captured immediately before closing an issue.
type NoDiffClose struct {
	Account     string `json:"account"`
	TimelineMax int64  `json:"timelineMax"`
	Head        string `json:"head"`
	Base        string `json:"base"`
	RequestHash string `json:"requestHash"`
	TargetBase  string `json:"targetBase"`
}

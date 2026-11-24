package journal

// ProviderFailure is controller evidence, never an instruction from the AI result.
type ProviderFailure struct {
	Kind       string `json:"kind"`
	At         string `json:"at"`
	Generation int    `json:"generation"`
	Detail     string `json:"detail"`
}
type ProviderSwitch struct {
	Cause     string            `json:"cause,omitempty"`
	From      string            `json:"from"`
	To        string            `json:"to"`
	Reason    string            `json:"reason"`
	At        string            `json:"at"`
	AppliedAt string            `json:"appliedAt,omitempty"`
	Failures  []ProviderFailure `json:"failures"`
}

type ProviderDenialHold struct {
	Version  string `json:"version"`
	At       string `json:"at"`
	Job      string `json:"job"`
	Count    int    `json:"count"`
	Inside   int    `json:"inside"`
	Pathless int    `json:"pathless"`
}
type ProviderDenied struct {
	Generation int      `json:"generation"`
	At         string   `json:"at"`
	Count      int      `json:"count"`
	Actions    []string `json:"actions"`
	Unexpected int      `json:"unexpected"`
	Inside     int      `json:"inside"`
	Pathless   int      `json:"pathless"`
}

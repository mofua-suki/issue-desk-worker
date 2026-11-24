// Package jobspec is the job descriptor the intake hands out with a lease:
// the Issue, repository, provider, generation and the intake's view of the
// job's progress.
package jobspec

import "encoding/json"

// Descriptor is the job as the intake describes it. execution embeds it; a
// MarshalJSON or UnmarshalJSON here would also take over the embedding type.
type Descriptor struct {
	ID         int64                      `json:"id"`
	Repo       string                     `json:"repo"`
	Issue      int64                      `json:"issue"`
	Title      string                     `json:"title"`
	Kind       string                     `json:"kind"`
	Provider   string                     `json:"provider"`
	Tier       string                     `json:"tier"`
	Generation int                        `json:"generation"`
	Branch     string                     `json:"branch"`
	HeadSHA    string                     `json:"headSha"`
	PR         int64                      `json:"pr"`
	View       map[string]json.RawMessage `json:"view"`
	Result     json.RawMessage            `json:"result"`
	Scope      []string                   `json:"scope"`
}

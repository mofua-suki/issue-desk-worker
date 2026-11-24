package execution

import "issue-desk-worker/internal/redact"

const redactedMark = redact.Mark

// redactKnown hides the known credential formats. Use e.redact where an
// Executor is at hand, so the Gitea token itself is hidden too.
func redactKnown(s string) string { return redact.Known(s) }

// redact hides the worker's own Gitea token and the known credential formats.
func (e *Executor) redact(s string) string {
	if e == nil {
		return redact.Known(s)
	}
	return redact.Secret(s, e.secret)
}

// containsCredential reports whether publishing s would expose the Gitea token
// or a whole credential. Short fragments are not a refusal.
func (e *Executor) containsCredential(s string) bool {
	if e == nil {
		return redact.HasCredential(s, "")
	}
	return redact.HasCredential(s, e.secret)
}

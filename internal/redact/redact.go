// Package redact hides credentials in text the Worker records, logs or
// publishes.
package redact

import (
	"regexp"
	"strings"
)

// credentialTokens are whole credentials: intake tokens (idk_/idh_), OpenAI and
// Anthropic keys (sk-) and GitHub tokens (gh?_), at least 20 characters. The
// word boundary keeps names such as "issue-desk-worker-…" readable.
// Publication checks refuse only these, so a short example such as idh_short
// in a proposal is not a refusal.
var credentialTokens = regexp.MustCompile(`id[kh]_[A-Za-z0-9_-]{20,}|\bsk-[A-Za-z0-9_-]{20,}|\bgh[pousr]_[A-Za-z0-9]{20,}`)

// tokenFragments also hide shorter pieces of intake and Anthropic tokens in
// what the worker records or logs.
var tokenFragments = regexp.MustCompile(`\bid[kh]_[A-Za-z0-9_-]+|sk-ant-[A-Za-z0-9_-]+`)

// Mark replaces what is hidden.
const Mark = "[REDACTED]"

// Known hides the known credential formats.
func Known(s string) string {
	return tokenFragments.ReplaceAllString(credentialTokens.ReplaceAllString(s, Mark), Mark)
}

// Secret hides secret itself, then the known credential formats.
func Secret(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, Mark)
	}
	return Known(s)
}

// HasCredential reports whether s contains secret or a whole credential.
// Short fragments do not count.
func HasCredential(s, secret string) bool {
	return secret != "" && strings.Contains(s, secret) || credentialTokens.MatchString(s)
}

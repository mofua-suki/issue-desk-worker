package execution

import (
	"errors"
	"issue-desk-worker/internal/gitea"
)

// Only a definite HTTP refusal of this POST permits clearing its durable attempt.
func rejectedPost(err error) bool {
	var refusal *gitea.Error
	return errors.As(err, &refusal) && refusal.Status >= 400 && refusal.Status < 500 && refusal.Status != 408
}

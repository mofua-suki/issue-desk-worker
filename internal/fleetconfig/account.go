package fleetconfig

import (
	"context"
	"errors"
	"fmt"

	"issue-desk-worker/internal/gitea"
)

// VerifyWorkerAccount checks that the Gitea token belongs to the monitoring
// account assigned to c.Host.
func VerifyWorkerAccount(ctx context.Context, c Configuration, g *gitea.Client) error {
	expected := c.Assignments.WorkerAccounts[c.Host]
	if expected == "" {
		return errors.New("workerAccountsにこのPCの監視アカウントがありません")
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := g.Request(ctx, "GET", "/user", nil, &user); err != nil {
		return fmt.Errorf("対象Giteaの監視アカウントを検証できません: %w", err)
	}
	if user.Login != expected {
		return errors.New("Gitea監視アカウントがworkerAccountsと一致しません")
	}
	return nil
}

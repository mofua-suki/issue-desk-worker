package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/cause"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var containerIDPattern = regexp.MustCompile(`^[0-9a-f]{12,64}$`)
var imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (r *Runtime) workerCompose(ctx context.Context, operation ...string) (string, error) {
	args := []string{"compose", "--project-directory", r.Config.ClientDir, "--env-file", filepath.Join(r.Config.ClientDir, ".env"), "-f", filepath.Join(r.Config.ClientDir, "compose.yaml")}
	return r.command(ctx, r.Config.ClientDir, "docker", append(args, operation...), "", 2*time.Minute)
}

func (r *Runtime) workerIDs(ctx context.Context) ([]string, error) {
	raw, err := r.workerCompose(ctx, "ps", "--all", "--quiet", "issue-desk-client")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(raw)
	if len(ids) == 0 {
		return nil, errors.New("Workerコンテナを確認できません")
	}
	for _, id := range ids {
		if !containerIDPattern.MatchString(id) {
			return nil, errors.New("WorkerコンテナのIDが不正です")
		}
	}
	return ids, nil
}

func (r *Runtime) stopWorker(ctx context.Context) error {
	// The shipped compose/image use the Docker default SIGTERM, with 60s grace.
	ids, err := r.workerIDs(ctx)
	if err != nil {
		return err
	}
	if _, err := r.workerCompose(ctx, "stop", "--timeout", "60", "issue-desk-client"); err != nil {
		return err
	}
	after, err := r.workerIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range append(ids, after...) {
		raw, err := r.command(ctx, r.Config.ClientDir, "docker", []string{"inspect", "--format", "{{json .State}}", id}, "", time.Minute)
		var state struct {
			Running    *bool  `json:"Running"`
			Restarting *bool  `json:"Restarting"`
			Status     string `json:"Status"`
		}
		if err != nil || json.Unmarshal([]byte(raw), &state) != nil || state.Running == nil || state.Restarting == nil || *state.Running || *state.Restarting || (state.Status != "exited" && state.Status != "dead" && state.Status != "created") {
			return cause.New("Workerコンテナの停止を確認できません", err)
		}
	}
	return nil
}

func (r *Runtime) verifyRunningDigest(ctx context.Context, target Target) bool {
	ids, err := r.workerIDs(ctx)
	if err != nil {
		return false
	}
	for _, id := range ids {
		raw, err := r.command(ctx, r.Config.ClientDir, "docker", []string{"inspect", "--format", "{{json .}}", id}, "", time.Minute)
		var container struct {
			Image string
			State struct {
				Running bool
			}
		}
		if err != nil || json.Unmarshal([]byte(raw), &container) != nil || !container.State.Running || !imageDigestPattern.MatchString(container.Image) {
			return false
		}
		digests, err := r.imageDigests(ctx, container.Image)
		if err != nil {
			return false
		}
		found := false
		for _, digest := range digests {
			found = found || digest == target.Registry+"/issue-desk-client"+target.Flavor+"@"+target.Digest
		}
		if !found {
			return false
		}
	}
	return true
}

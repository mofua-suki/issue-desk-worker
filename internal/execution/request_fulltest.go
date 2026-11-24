package execution

import (
	"context"
	"encoding/json"
	"time"

	"issue-desk-worker/internal/journal"
)

func (e *Executor) fullTestRequest(ctx context.Context, r monitorRequest) (map[string]any, error) {
	if string(r.Payload) != "{}" {
		return map[string]any{"status": "unrunnable"}, nil
	}
	info, err := e.Gitea.Repo(ctx, r.Repo)
	if err != nil {
		return nil, err
	}
	w, err := e.requestWorkspace(ctx, r, info.DefaultBranch, "chat/full-test-"+r.ID)
	if err != nil {
		return nil, err
	}
	if _, err := w.Follow(ctx); err != nil {
		return nil, err
	}
	head, err := w.Head(ctx)
	if err != nil {
		return nil, err
	}
	config, err := e.Config.Repository(r.Repo)
	if err != nil {
		return nil, err
	}
	record := &journal.Record{Repo: r.Repo, Base: head, Tree: w.Tree, Result: json.RawMessage(`{}`)}
	started := time.Now().UTC()
	if err := e.setup(ctx, config, record); err != nil {
		return map[string]any{"status": "unrunnable", "excerpt": fixedTestMessage("", e.redact(err.Error()), 3000, true)}, nil
	}
	tested, err := e.measuredTest(ctx, config, record, "full-test")
	if err != nil {
		return nil, err
	}
	actual, headErr := e.testHead(ctx, record)
	if headErr != nil || actual != head {
		rejectTestEvidence(&tested, "全テストの対象headを照合できないか変更されました")
	}
	status := "failed"
	files := 0
	names := []string{}
	failed := []string{}
	if tested.Passed && w.Clean(ctx) {
		status = "passed"
		files = fullTestFiles(tested)
	} else {
		if observed, ok := tested.Observation["failures"].([]any); ok {
			for _, raw := range observed {
				if f, ok := raw.(map[string]any); ok {
					if name, ok := f["test"].(string); ok {
						names = append(names, name)
						failed = append(failed, name)
					}
				}
			}
		}
	}
	return map[string]any{"status": status, "sha": head, "startedAt": started.Format("2006-01-02T15:04:05.000Z"), "finishedAt": tested.Observation["at"], "files": files, "ms": time.Since(started).Milliseconds(), "failedFiles": failed, "excerpt": failureExcerpt(e.redact(tested.Detail), 3000), "execution": map[string]any{"command": tested.Observation["command"], "environment": tested.Observation["environment"], "baseSha": head}, "causeObservation": tested.Observation, "fixedTestCause": e.classifyTest(r.Repo, tested.Observation), "failures": map[string]any{"names": names, "complete": tested.Observation["complete"] == true}}, nil
}

// fullTestFiles reports the completed count recorded in successful pass evidence.
func fullTestFiles(tested TestResult) int {
	if proof, ok := tested.Observation["passEvidence"].(map[string]any); ok && tested.Passed {
		if record, ok := proof["record"].(map[string]any); ok {
			switch files := record["files"].(type) {
			case int:
				return files
			case float64:
				return int(files)
			}
		}
	}
	return 0
}

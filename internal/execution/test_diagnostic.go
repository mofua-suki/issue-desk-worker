package execution

import (
	"context"
	"errors"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/process"
	"strings"
	"time"
)

// TestDiagnostic is the controller's measured context, never an AI completion claim.
// It travels with r.Test into repair input, restart journals and the final report.
type TestDiagnostic struct {
	Version        int            `json:"version"`
	Source         string         `json:"source"`
	Head           any            `json:"head"`
	Base           string         `json:"base"`
	Command        []string       `json:"command"`
	Environment    map[string]any `json:"environment"`
	Started        string         `json:"startedAt"`
	Finished       string         `json:"finishedAt"`
	Timeout        int            `json:"timeoutSeconds"`
	Reason         string         `json:"reason"`
	Code           int            `json:"code"`
	StartedProcess bool           `json:"processStarted"`
	Termination    string         `json:"termination"`
	UserMillis     *int64         `json:"userMillis"`
	SystemMillis   *int64         `json:"systemMillis"`
	Stages         []string       `json:"stages"`
	Excerpt        string         `json:"excerpt"`
}

func (e *Executor) testDiagnostic(ctx context.Context, r *journal.Record, source string, command []string, observation, environment map[string]any, out process.Result, runErr error, started, finished time.Time, timeout int) *TestDiagnostic {
	reason := "passed"
	switch {
	case errors.Is(runErr, process.ErrOutputLimit):
		reason = "output_limit"
	case errors.Is(runErr, context.DeadlineExceeded):
		reason = "timeout"
	case ctx.Err() != nil:
		reason = "canceled"
	case out.Interrupted:
		reason = "interrupted"
	case !out.Started:
		reason = "not_started"
	case runErr != nil:
		reason = "execution_error"
	case out.Code != 0:
		reason = "test_failure"
	}
	output := e.redact(out.Out + "\n" + out.Err)
	if runErr != nil {
		output += "\n" + e.redact(runErr.Error())
	}
	stages := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimPrefix(line, "# ")
		if strings.HasPrefix(line, "stage=") {
			stages = append(stages, clip(line, 300))
		}
	}
	if len(stages) > 20 {
		stages = stages[len(stages)-20:]
	}
	resolved := e.resolveTestCommand(command)
	for i, v := range resolved {
		resolved[i] = clip(e.redact(v), 500)
	}
	if len(resolved) > 40 {
		resolved = resolved[:40]
	}
	// Redact measured environment values too; never include the process environment.
	safe := map[string]any{}
	for key, value := range environment {
		if text, ok := value.(string); ok {
			safe[key] = e.redact(text)
		} else {
			safe[key] = value
		}
	}
	code := out.Code
	if runErr != nil && code == 0 {
		code = -1
	}
	return &TestDiagnostic{Version: 1, Source: source, Head: observation["sha"], Base: r.Base, Command: resolved, Environment: safe, Started: started.UTC().Format(time.RFC3339Nano), Finished: finished.UTC().Format(time.RFC3339Nano), Timeout: timeout, Reason: reason, Code: code, StartedProcess: out.Started, Termination: out.Termination, UserMillis: out.UserMillis, SystemMillis: out.SystemMillis, Stages: stages, Excerpt: failureExcerpt(output, 6000)}
}

func safeTestCommand(e *Executor, command []string) []string {
	out := e.resolveTestCommand(command)
	for i, text := range out {
		out[i] = e.redact(text)
	}
	return out
}

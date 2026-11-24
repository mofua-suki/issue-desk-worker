package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/failurelog"
	"net/url"
	"sync"
	"time"
)

// This file polls the intake for job commands (stop, instruction,
// repost_proposal) and hands each one to the job it names.

// commandLoop is the state of the command polling started by StartCommands.
// pollMu serializes a poll with notification recovery, which also acks
// repost commands.
type commandLoop struct {
	pollMu  sync.Mutex
	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	handled handledCommands
}

// handledCommands are commands a job acted on whose ack did not reach the
// intake. They are kept in memory only, at most handledCommandLimit of them.
type handledCommands struct {
	mu    sync.Mutex
	ids   map[string]bool
	order []string
}

const handledCommandLimit = 256

func (h *handledCommands) add(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ids == nil {
		h.ids = map[string]bool{}
	}
	if h.ids[id] {
		return
	}
	if len(h.order) >= handledCommandLimit {
		delete(h.ids, h.order[0])
		h.order = h.order[1:]
	}
	h.ids[id] = true
	h.order = append(h.order, id)
}

func (h *handledCommands) has(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ids[id]
}

func (h *handledCommands) remove(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.ids, id)
	for i, other := range h.order {
		if other == id {
			h.order = append(h.order[:i], h.order[i+1:]...)
			break
		}
	}
}

// ackCommand reports the outcome of one command to the intake.
func (e *Executor) ackCommand(ctx context.Context, id string, result map[string]any) error {
	return e.API.Call(ctx, "POST", "/api/v1/commands/"+url.PathEscape(id)+"/ack", map[string]any{"status": "done", "result": result}, nil)
}

type command struct {
	ID         string          `json:"id"`
	Kind       string          `json:"kind"`
	Repo       string          `json:"repo"`
	Issue      json.RawMessage `json:"issue"`
	TargetHost string          `json:"targetHost"`
	Payload    struct {
		JobID      int64           `json:"jobId"`
		Epoch      int64           `json:"epoch"`
		Text       string          `json:"text"`
		Context    string          `json:"context"`
		V          json.RawMessage `json:"v,omitempty"`
		CommentID  json.RawMessage `json:"commentId,omitempty"`
		ProposalID json.RawMessage `json:"proposalId,omitempty"`
	} `json:"payload"`
}

// Keep malformed repost values local to that command: a bad payload must be
// refused, rather than making the entire commands response undecodable. The
// existing job commands retain their previous decoding rules.
func (c *command) UnmarshalJSON(raw []byte) error {
	type wireCommand command
	var header struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return err
	}
	if header.Kind != "repost_proposal" {
		return json.Unmarshal(raw, (*wireCommand)(c))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	*c = command{Kind: header.Kind}
	if err := json.Unmarshal(fields["id"], &c.ID); err != nil {
		return err
	}
	_ = json.Unmarshal(fields["repo"], &c.Repo)
	_ = json.Unmarshal(fields["targetHost"], &c.TargetHost)
	c.Issue = fields["issue"]
	var payload map[string]json.RawMessage
	if json.Unmarshal(fields["payload"], &payload) == nil {
		c.Payload.V = payload["v"]
		c.Payload.CommentID = payload["commentId"]
		c.Payload.ProposalID = payload["proposalId"]
	}
	return nil
}

// commandFailureLog records command polling failures without credentials.
func commandFailureLog() *failurelog.Log {
	return &failurelog.Log{Label: "命令を確認できませんでした", Redact: redactKnown}
}

// StartCommands polls the intake for job commands every two seconds until ctx
// ends or StopCommands is called. A second call while polling does nothing.
func (e *Executor) StartCommands(ctx context.Context) {
	e.commands.mu.Lock()
	defer e.commands.mu.Unlock()
	if e.commands.cancel != nil {
		return
	}
	polling, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	e.commands.cancel, e.commands.done = cancel, done
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		failures := commandFailureLog()
		for {
			select {
			case <-polling.Done():
				return
			case <-ticker.C:
				err := e.pollCommands(polling)
				if polling.Err() == nil {
					failures.Observe(err)
				}
			}
		}
	}()
}

// StopCommands ends the polling started by StartCommands and waits for an
// in-flight poll to return.
func (e *Executor) StopCommands() {
	e.commands.mu.Lock()
	cancel, done := e.commands.cancel, e.commands.done
	e.commands.cancel, e.commands.done = nil, nil
	e.commands.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// jobCommandError is a command of one job that could not be handled.
type jobCommandError struct {
	job int64
	err error
}

func (e *jobCommandError) Error() string { return e.err.Error() }
func (e *jobCommandError) Unwrap() error { return e.err }

// pollCommandsFor polls the commands for a running job before it publishes or
// merges. A command of another job that cannot be handled is that job's
// problem and is retried by the background poll; it does not stop this job.
// Failing to read the commands at all still does, so a stop is not missed.
func (e *Executor) pollCommandsFor(ctx context.Context, job int64) error {
	err := e.pollCommands(ctx)
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		var single *jobCommandError
		if errors.As(err, &single) && single.job != job {
			return nil
		}
		return err
	}
	var own []error
	for _, failure := range joined.Unwrap() {
		var other *jobCommandError
		if errors.As(failure, &other) && other.job != job {
			continue
		}
		own = append(own, failure)
	}
	return errors.Join(own...)
}

func (e *Executor) pollCommands(ctx context.Context) error {
	e.commands.pollMu.Lock()
	defer e.commands.pollMu.Unlock()
	var response struct {
		Commands []command `json:"commands"`
		Next     int64     `json:"next"`
	}
	if err := e.API.Call(ctx, "GET", "/api/v1/commands?host="+url.QueryEscape(e.Config.Host)+"&after=0", nil, &response); err != nil {
		return err
	}
	// A failed command is retried on the next poll. Later commands of the same
	// job wait for it so instructions keep their order; other jobs go on.
	var failures []error
	blocked := map[int64]bool{}
	fail := func(cmd command, err error) {
		blocked[cmd.Payload.JobID] = true
		failures = append(failures, &jobCommandError{job: cmd.Payload.JobID, err: fmt.Errorf("命令%s（仕事%d）: %w", clip(cmd.ID, 80), cmd.Payload.JobID, err)})
	}
	for _, cmd := range response.Commands {
		if ctx.Err() != nil {
			failures = append(failures, ctx.Err())
			break
		}
		handler, known := commandKinds[cmd.Kind]
		if !known {
			continue
		}
		if handler.Repost {
			// Reposts are independent of job control. Leave transient failures
			// unacked for the next poll without failing a job or hiding its stop.
			_ = e.repostProposal(ctx, cmd)
			continue
		}
		if blocked[cmd.Payload.JobID] {
			continue
		}
		if e.commands.handled.has(cmd.ID) {
			// The job already acted on this command, but its ack was lost.
			// Report it as handled, not as a command for a job no longer held.
			if e.ackCommand(ctx, cmd.ID, map[string]any{"handled": true}) == nil {
				e.commands.handled.remove(cmd.ID)
			}
			continue
		}
		active := e.controls.get(cmd.Payload.JobID)
		if active == nil || active.epoch != cmd.Payload.Epoch {
			var current struct {
				Job struct {
					ID         int64  `json:"id"`
					Status     string `json:"status"`
					LeaseHost  string `json:"leaseHost"`
					LeaseEpoch int64  `json:"leaseEpoch"`
				}
			}
			if err := e.API.Call(ctx, "GET", fmt.Sprintf("/api/v1/jobs/%d", cmd.Payload.JobID), nil, &current); err != nil {
				fail(cmd, err)
				continue
			}
			if current.Job.ID == cmd.Payload.JobID && current.Job.Status == "running" && current.Job.LeaseHost == e.Config.Host && current.Job.LeaseEpoch == cmd.Payload.Epoch {
				continue
			}
		}
		if active == nil && cmd.Kind == "instruction" {
			record, loadErr := e.Store.Load(cmd.Payload.JobID)
			if loadErr != nil {
				fail(cmd, loadErr)
				continue
			}
			if record != nil && record.Epoch == cmd.Payload.Epoch && record.Stage != "done" {
				if err := e.saveInstruction(cmd); err != nil {
					fail(cmd, err)
					continue
				}
				_ = e.ackCommand(ctx, cmd.ID, map[string]any{"savedForNextLease": true})
				continue
			}
		}
		if active == nil || active.epoch != cmd.Payload.Epoch {
			_ = e.ackCommand(ctx, cmd.ID, map[string]any{"ignored": "not_held_or_stale_epoch"})
			continue
		}
		active.mu.Lock()
		if active.mergeStarted {
			active.mu.Unlock()
			_ = e.ackCommand(ctx, cmd.ID, map[string]any{"ignored": "merge_started", "requiresNewIssue": cmd.Kind == "instruction"})
			continue
		}
		found := false
		for _, id := range active.ids {
			if id == cmd.ID {
				found = true
				break
			}
		}
		if !found {
			if cmd.Kind == "instruction" {
				if err := e.saveInstruction(cmd); err != nil {
					active.mu.Unlock()
					fail(cmd, err)
					continue
				}
			}
			active.ids = append(active.ids, cmd.ID)
			if cmd.Kind == "stop" || active.reason == "" {
				active.reason = cmd.Kind
			}
			active.cancel()
		}
		active.mu.Unlock()
	}
	return errors.Join(failures...)
}

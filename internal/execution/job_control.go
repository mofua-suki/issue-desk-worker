package execution

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/journal"
)

// This file runs one leased job and keeps the controls that stop or
// interrupt it. Command polling (command_poll.go) only finds a job's control
// and signals it; the job itself reports the result.

type control struct {
	mu           sync.Mutex
	epoch        int64
	cancel       context.CancelFunc
	reason       string
	ids          []string
	mergeStarted bool
}

// jobControls are the controls of the jobs this Executor is running, by job ID.
type jobControls struct {
	mu   sync.Mutex
	byID map[int64]*control
}

func (c *jobControls) add(id int64, active *control) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = map[int64]*control{}
	}
	c.byID[id] = active
}

func (c *jobControls) remove(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byID, id)
}

func (c *jobControls) get(id int64) *control {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byID[id]
}

func (c *jobControls) ids() []int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]int64, 0, len(c.byID))
	for id := range c.byID {
		ids = append(ids, id)
	}
	return ids
}

func (e *Executor) Execute(ctx context.Context, job intake.Job, fence func(context.Context) error) (result intake.State, returned error) {
	var descriptor Descriptor
	_ = json.Unmarshal(job.Raw, &descriptor)
	tracker := &failureRecord{generation: descriptor.Generation, epoch: job.LeaseEpoch}
	ctx = context.WithValue(ctx, failureRecordKey{}, tracker)
	defer func() {
		if returned != nil {
			returned = &ExecutionError{Err: returned, Stage: failureStage(tracker.record)}
		}
	}()
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	active := &control{epoch: job.LeaseEpoch, cancel: cancel}
	e.controls.add(job.ID, active)
	defer e.controls.remove(job.ID)
	state, err := e.execute(workCtx, job, fence)
	if errors.Is(err, intake.ErrJobReleased) {
		return state, err
	}
	if ctx.Err() != nil {
		return state, ctx.Err()
	}
	active.mu.Lock()
	reason := active.reason
	ids := append([]string{}, active.ids...)
	active.mu.Unlock()
	if reason != "" {
		r, loadErr := e.Store.Load(job.ID)
		if loadErr != nil {
			return intake.State{}, loadErr
		}
		if r == nil {
			r = &journal.Record{ID: job.ID, Epoch: job.LeaseEpoch, Repo: job.Repo, Issue: descriptor.Issue, Generation: descriptor.Generation, Stage: "stopped"}
			if reason == "stop" && descriptor.Generation == 0 && job.LeaseEpoch == 1 && e.Config.Host != "" && descriptor.Branch == "" && descriptor.HeadSHA == "" && descriptor.PR == 0 && !hasJSON(descriptor.Result) && !hasJSON(descriptor.View["workerExecution"]) {
				r.StoppedBeforeStart = true
				observeStartHistory(r, descriptor.Generation, job.LeaseEpoch, e.Config.Host, true)
			}
		}
		trackFailureRecord(ctx, r)
		status := "stopped"
		summary := "受付の停止指示を反映しました"
		if reason == "instruction" {
			status = "queued"
			summary = "追加指示を反映するため実行を中断し、保存した工程から再開します"
			r.Stage = "implement"
			r.ReviewBaseline = nil
			r.CarriedHeldReview = nil
			r.ReviewSkipped = false
			keepPreviousResults(r)
			r.Result = nil
			r.Review = nil
			r.Test = nil
		}
		state, err = e.finish(r, status, "", summary, nil)
		if err == nil && reason == "instruction" {
			r.Stage = "implement"
			err = e.Store.Save(r)
		}
		for _, id := range ids {
			// Remember the command before acking, so a poll that runs while
			// the ack is in flight already reports it as handled.
			e.commands.handled.add(id)
			if e.ackCommand(ctx, id, map[string]any{"handled": true}) == nil {
				e.commands.handled.remove(id)
			}
		}
	}
	if err == nil && state.Status != "running" {
		record, loadErr := e.Store.Load(job.ID)
		if loadErr != nil {
			return state, loadErr
		}
		if record != nil {
			trackFailureRecord(ctx, record)
			state, err = e.resultNotice(ctx, record, job.LeaseEpoch, state)
		}
	}
	return state, err
}

func (e *Executor) markMergeStarted(id int64) error {
	active := e.controls.get(id)
	if active == nil {
		return nil
	}
	active.mu.Lock()
	defer active.mu.Unlock()
	if active.reason != "" {
		return context.Canceled
	}
	active.mergeStarted = true
	return nil
}

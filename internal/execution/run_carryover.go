package execution

import (
	"encoding/json"
	"time"

	"issue-desk-worker/internal/journal"
)

// maxInterruptedRuns bounds how many interrupted starts one Issue may take back.
// Beyond it, a job that keeps killing its Worker counts UnansweredRuns: it has no
// usage rows to stop it and must still reach the run limit.
const maxInterruptedRuns = 2

// startedExecution is the Worker's own checkpoint in the job view.
type startedExecution struct {
	StartedGenerations              []int `json:"startedGenerations"`
	ContinuationExcludedGenerations []int `json:"continuationExcludedGenerations"`
	OtherExcludedGenerations        []int `json:"otherExcludedGenerations"`
	StartHistoryGeneration          *int  `json:"startHistoryGeneration"`
	Runs                            int   `json:"implementationRuns"`
	ReportCompletionRuns            int   `json:"reportCompletionRuns"`
	OpenRun                         int   `json:"openRun"`
	OpenRunGeneration               int   `json:"openRunGeneration"`
	InterruptedRuns                 int   `json:"interruptedRuns"`
	UnansweredRuns                  int   `json:"unansweredRuns"`
	ReviewReworkRuns                int   `json:"reviewReworkRuns"`
	// Generation is absent in checkpoints from older Workers.
	Generation          *int  `json:"generation"`
	ExcludedGenerations []int `json:"excludedGenerations"`
	ResumedGenerations  []int `json:"resumedGenerations"`
	ResumesSeen         int   `json:"resumesSeen"`
	// UsageTrigger is the trigger already sent for a row generation (see addGenerationTrigger).
	UsageTrigger *journal.GenerationTrigger `json:"usageTrigger"`
}

func workerExecutionView(r *journal.Record, stage string) map[string]any {
	view := map[string]any{"stage": stage, "implementationRuns": r.Runs, "reportCompletionRuns": r.ReportCompletionRuns, "weightedUsage": r.WeightedUsage, "generation": r.Generation}
	if r.Timing != nil && !r.TimingPending && r.Timing.Generation == r.Generation && r.Timing.Epoch == r.Epoch {
		timing := r.Timing.SnapshotAt(time.Now())
		if len(timing.Millis) > 0 {
			view["stageTimingMillis"] = timing.Millis
		}
		if timing.Gaps {
			view["stageTimingGaps"] = true
		}
		if timing.AIRepairMillis != nil {
			view["aiRepairMillis"] = *timing.AIRepairMillis
		}
		if timing.FixedTestCount > 0 {
			view["fixedTestCount"] = timing.FixedTestCount
			count := len(timing.FixedTests)
			if count > journal.MaxFixedTestTimings {
				count = journal.MaxFixedTestTimings
			}
			view["fixedTests"] = append([]journal.FixedTestTiming(nil), timing.FixedTests[:count]...)
		}
		if len(timing.TestSteps) > 0 {
			steps := timing.TestSteps
			if len(steps) > journal.MaxFixedTestTimings*5 {
				steps = steps[:journal.MaxFixedTestTimings*5]
			}
			view["testSteps"] = steps
		}
	}
	if r.UsageTrigger != nil {
		view["usageTrigger"] = r.UsageTrigger
	}
	view["startedGenerations"] = append([]int{}, r.StartedGenerations...)
	view["continuationExcludedGenerations"] = r.ContinuationExcludedGenerations
	view["otherExcludedGenerations"] = r.OtherExcludedGenerations
	if r.StartHistory != nil {
		view["startHistoryGeneration"] = r.StartHistory.Generation
	}
	if r.OpenRun > 0 {
		view["openRun"], view["openRunGeneration"] = r.OpenRun, r.OpenRunGeneration
	}
	if r.InterruptedRuns > 0 {
		view["interruptedRuns"] = r.InterruptedRuns
	}
	if r.UnansweredRuns > 0 {
		view["unansweredRuns"] = r.UnansweredRuns
	}
	if r.ReviewReworkRuns > 0 {
		view["reviewReworkRuns"] = r.ReviewReworkRuns
	}
	if len(r.ExcludedGenerations) > 0 {
		view["excludedGenerations"] = r.ExcludedGenerations
	}
	if len(r.ResumedGenerations) > 0 {
		view["resumedGenerations"], view["resumesSeen"] = r.ResumedGenerations, r.ResumesSeen
	}
	return view
}

// reclaimInterruptedRun takes back a start that never returned an answer: the
// Worker restarted, the lease was lost, or an instruction cancelled the call.
// It reports whether a run was taken back, so the usage check stays automatic.
func reclaimInterruptedRun(d Descriptor, r *journal.Record, started startedExecution) bool {
	if started.InterruptedRuns > r.InterruptedRuns {
		r.InterruptedRuns = started.InterruptedRuns
	}
	open, generation := r.OpenRun, r.OpenRunGeneration
	rejected := rejectedOpenRun(r)
	if open == 0 {
		open, generation = started.OpenRun, started.OpenRunGeneration
	}
	r.OpenRun, r.OpenRunGeneration = 0, 0
	r.OpenRunEpoch = 0
	r.RejectedStart = nil
	if open < 1 || generation != d.Generation || open != r.Runs || r.Stage != "implement" {
		return false
	}
	if rejected {
		r.Runs--
		return true
	}
	if r.InterruptedRuns >= maxInterruptedRuns {
		r.UnansweredRuns++
		r.Runs++
		return false
	}
	r.Runs--
	r.InterruptedRuns++
	return true
}

func keepPreviousResults(r *journal.Record) {
	if hasJSON(r.Result) {
		r.PreviousResult = r.Result
	}
	rememberReview(r)
}

func usableCarried(raw json.RawMessage) bool {
	if !hasJSON(raw) {
		return false
	}
	var object struct {
		Omitted bool `json:"omitted"`
	}
	return json.Unmarshal(raw, &object) == nil && !object.Omitted
}

// seedPreviousResults reads the last report the intake kept. The journal stays
// on the PC that ran the job, so a job moved to another PC has only this copy.
func seedPreviousResults(r *journal.Record, d Descriptor) {
	var last struct {
		Result         json.RawMessage `json:"implementationResult"`
		PreviousResult json.RawMessage `json:"previousImplementationResult"`
		Review         json.RawMessage `json:"review"`
		PreviousReview json.RawMessage `json:"previousReview"`
	}
	if !hasJSON(d.Result) || json.Unmarshal(d.Result, &last) != nil {
		return
	}
	for _, candidate := range []json.RawMessage{last.Result, last.PreviousResult} {
		if usableCarried(candidate) {
			r.PreviousResult = candidate
			break
		}
	}
	seedPreviousReview(r, d, last.Review, last.PreviousReview)
}

// carryReviewCounts keeps the review counts the intake was last sent, so a
// job moved to another PC, which starts without the journal, goes on counting
// from them. Each count only grows, so the larger of the two is kept.
func carryReviewCounts(r *journal.Record, d Descriptor) {
	var last struct {
		Counts *journal.ReviewCounts `json:"reviewCounts"`
	}
	if !hasJSON(d.Result) || json.Unmarshal(d.Result, &last) != nil || last.Counts == nil {
		return
	}
	r.EnsureReviewCounts()
	r.ReviewCounts.Full = max(r.ReviewCounts.Full, last.Counts.Full)
	r.ReviewCounts.Delta = max(r.ReviewCounts.Delta, last.Counts.Delta)
	r.ReviewCounts.Skipped = max(r.ReviewCounts.Skipped, last.Counts.Skipped)
}

func firstJSON(values ...json.RawMessage) json.RawMessage {
	for _, value := range values {
		if hasJSON(value) {
			return value
		}
	}
	return nil
}

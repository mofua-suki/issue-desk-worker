package journal

import "time"

// StageTiming contains only intervals observed in this process. Missing map keys
// are unobserved, not zero. Gaps includes time before tracking and after a crash.
type StageTiming struct {
	Generation     int               `json:"generation"`
	Epoch          int64             `json:"epoch"`
	Stage          string            `json:"stage,omitempty"`
	LastStage      string            `json:"lastStage,omitempty"`
	Since          time.Time         `json:"since"`
	StartedAt      time.Time         `json:"startedAt"`
	Millis         map[string]int64  `json:"millis"`
	Gaps           bool              `json:"gaps"`
	Intervals      []StageInterval   `json:"intervals,omitempty"`
	TestSteps      []TestStepTiming  `json:"testSteps,omitempty"`
	TestAttempts   map[string]int    `json:"testAttempts,omitempty"`
	AIRepairMillis *int64            `json:"aiRepairMillis,omitempty"`
	FixedTestCount int               `json:"fixedTestCount,omitempty"`
	FixedTests     []FixedTestTiming `json:"fixedTests,omitempty"`
}

// FixedTestTiming holds the final result of one fixed-test invocation.
type FixedTestTiming struct {
	ElapsedMS *int64 `json:"elapsedMs,omitempty"`
	Passed    bool   `json:"passed"`
}

const MaxFixedTestTimings = 100

// TestStepTiming distinguishes an unobserved duration from an observed zero.
type TestStepTiming struct {
	WorkerID      string `json:"workerId"`
	Step          string `json:"step"`
	Attempt       int    `json:"attempt"`
	Status        string `json:"status"`
	ElapsedMillis *int64 `json:"elapsedMillis,omitempty"`
}

type StageInterval struct {
	Stage string    `json:"stage"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

func stageClass(stage string) string {
	switch stage {
	case "implement", "implement_report":
		return "implementing"
	case "test", "test_report":
		return "test"
	case "review", "publish", "merge", "deploy":
		return stage
	default:
		return ""
	}
}

// StartStageTiming deliberately excludes the interval since the last process
// stopped: a persisted timestamp cannot establish when execution stopped.
func (r *Record) StartStageTiming(generation int, epoch int64) {
	now := time.Now().UTC()
	if r.Timing == nil {
		// A loaded legacy record has no timing history for its current generation.
		gap := (r.loaded && !r.freshlySaved && r.Generation == generation) || (!r.loaded && r.Stage != "" && r.Stage != "implement")
		r.Timing = &StageTiming{Generation: generation, Epoch: epoch, Millis: map[string]int64{}, Gaps: gap}
	} else if r.Timing.Generation != generation {
		// A new generation starts a separate observation, including after retry reset.
		r.Timing = &StageTiming{Generation: generation, Epoch: epoch, Millis: map[string]int64{}}
	} else if r.Timing.Epoch != epoch {
		r.Timing = &StageTiming{Generation: generation, Epoch: epoch, Millis: map[string]int64{}, Gaps: true}
	} else if !r.timingRunning {
		r.Timing.Gaps = true
		r.Timing.Epoch = epoch
	}
	r.TimingPending = false
	r.freshlySaved = false
	if r.Timing.Millis == nil {
		r.Timing.Millis = map[string]int64{}
	}
	r.Timing.Stage = r.Stage
	r.Timing.LastStage = r.Stage
	r.Timing.Since = now
	r.Timing.StartedAt = now
	r.timingRunning = true
}

func (r *Record) observeStage(now time.Time) {
	if !r.timingRunning || r.Timing == nil {
		return
	}
	t := r.Timing
	if now.Before(t.Since) {
		t.Gaps = true
	} else if class := stageClass(t.Stage); class != "" {
		t.Millis[class] += now.Sub(t.Since).Milliseconds()
		n := len(t.Intervals)
		if n > 0 && t.Intervals[n-1].Stage == t.Stage && t.Intervals[n-1].End.Equal(t.Since) {
			t.Intervals[n-1].End = now
		} else {
			t.Intervals = append(t.Intervals, StageInterval{Stage: t.Stage, Start: t.Since, End: now})
		}
	}
	if stageClass(t.Stage) != "" {
		t.LastStage = t.Stage
	}
	t.Stage = r.Stage
	if stageClass(r.Stage) != "" {
		t.LastStage = r.Stage
	}
	t.Since = now
}

// SnapshotAt adds the observed tail between the last checkpoint and an event.
// Call only for the same live execution, never while resuming after a crash.
func (t StageTiming) SnapshotAt(at time.Time) StageTiming {
	copy := t
	copy.Millis = make(map[string]int64)
	copy.LastStage = ""
	for _, interval := range t.Intervals {
		if at.Before(interval.Start) {
			continue
		}
		end := interval.End
		if at.Before(end) {
			end = at
		}
		if class := stageClass(interval.Stage); class != "" && !end.Before(interval.Start) {
			copy.Millis[class] += end.Sub(interval.Start).Milliseconds()
			copy.LastStage = interval.Stage
		}
	}
	if !at.Before(t.Since) && stageClass(t.Stage) != "" {
		class := stageClass(t.Stage)
		copy.Millis[class] += at.Sub(t.Since).Milliseconds()
		copy.LastStage = t.Stage
	}
	return copy
}

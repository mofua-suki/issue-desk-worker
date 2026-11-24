package execution

import (
	"encoding/json"
	"slices"

	"issue-desk-worker/internal/journal"
)

// 実装の回数は、受付の ImplementationRuns と同じ「実装の世代」で数える。
//
//	世代の回数 = Generation + 1 - 除外した世代数 - implementationResumes（1未満は1）
//
// record.Runs は「開始済みの世代の回数 + 報告補完 + 答えが返らなかった開始（取り戻しの上限を超えた分）
// + 独立レビューの差し戻しからの再実装」で、受付へ workerExecution.implementationRuns として報告する値でもある。
// 受付はこの値と自分の式の大きい方を上限超過の承認に使うので、後ろ3つは上限と報告に含めたまま、世代の回数と分けて持つ。
// 差し戻しは受付の世代を進めないので、世代の式だけでは同じ実行の中の「実装→差し戻し→実装」が数えられない。
//
// 回答だけで変更の無い世代の扱い:
//   - 同じ世代のやり直し（lease の取り直し・回答の再開）は回数を増やさない。
//   - 独立レビューの差し戻しからの再実装は、開始の前に ReviewReworkRuns として1回数える。
//   - 受付が世代を進めた回答は、受付が再開と判定すれば implementationResumes で引かれ、
//     そうでなければコードの変更が無くても1世代として数える。受付の数え方と食い違わせないため。
//   - 回答の再開など回数で止まらない同じ世代の繰り返しの歯止めは、開始のたびの checkImplementationUsage（使用量の上限）と、
//     答えが返らない開始を数える UnansweredRuns で持つ。
//
// 除外の扱い:
//   - 受付の3つの一覧は受付と同じ条件で数える。
//   - この Worker が実装のせいでないと判定した世代（ExcludedGenerations）も引く。ただし受付が再開として
//     既に引いている世代（ResumedGenerations）は二重に引かない。
//   - 判定できない失敗は除外しない。

// nonGenerationRuns は record.Runs のうち世代の回数でない分を返す。
func nonGenerationRuns(r *journal.Record) int {
	return r.ReportCompletionRuns + r.UnansweredRuns + r.ReviewReworkRuns
}

// carryStartedRuns は別のPCの checkpoint（受付の workerExecution）にある回数のうち、手元より大きいものを取り込む。
// 世代の回数でない分は record.Runs を超えない値だけを信じる。
func carryStartedRuns(r *journal.Record, started startedExecution) {
	r.Runs = max(r.Runs, started.Runs)
	if started.ReportCompletionRuns <= started.Runs {
		r.ReportCompletionRuns = max(r.ReportCompletionRuns, started.ReportCompletionRuns)
	}
	r.UnansweredRuns = max(r.UnansweredRuns, started.UnansweredRuns)
	// The rows of a generation another PC began keep the trigger that PC sent for it.
	if started.UsageTrigger != nil && (r.UsageTrigger == nil || started.UsageTrigger.Generation > r.UsageTrigger.Generation) {
		r.UsageTrigger = started.UsageTrigger
	}
	if started.ReviewReworkRuns <= started.Runs {
		r.ReviewReworkRuns = max(r.ReviewReworkRuns, started.ReviewReworkRuns)
	}
}

// implementationRunCount は今の世代まで含めた世代の回数を返す。
func implementationRunCount(d Descriptor, r *journal.Record) int {
	excluded := map[int]bool{}
	for _, key := range []string{"retestPassedGenerations", "codexNotStartedGenerations", "prerequisiteMissingGenerations"} {
		for _, g := range field[[]int](d.View, key) {
			if g >= 0 && g <= d.Generation && (key != "retestPassedGenerations" || g > 0) {
				excluded[g] = true
			}
		}
	}
	for _, g := range r.ExcludedGenerations {
		if g >= 0 && g <= d.Generation && !slices.Contains(r.ResumedGenerations, g) {
			excluded[g] = true
		}
	}
	count := d.Generation + 1 - len(excluded) - field[int](d.View, "implementationResumes")
	if count < 1 {
		return 1
	}
	return count
}

// syncImplementationRuns は別のPCの記録を取り込み、record.Runs を世代の式へ計算し直す。旧 journal の
// 開始ごとに増えた値もここで下がる。今の世代を開始済みかどうかは保ち、未開始なら count-1 にする。
func syncImplementationRuns(d Descriptor, r *journal.Record, started startedExecution, newGeneration bool) int {
	classifyLocalExclusions(r)
	r.ExcludedGenerations = mergeGenerations(r.ExcludedGenerations, started.ExcludedGenerations)
	mergeStartEvidence(r, started)
	r.ResumedGenerations = mergeGenerations(r.ResumedGenerations, started.ResumedGenerations)
	if started.ResumesSeen > r.ResumesSeen {
		r.ResumesSeen = started.ResumesSeen
	}
	// 受付の再開回数が増えた世代を覚える。履歴の無い旧記録では今の世代に付くが、
	// その世代の除外が引かれなくなるだけで、回数は多い側へ倒れる。
	if resumes := field[int](d.View, "implementationResumes"); resumes > r.ResumesSeen {
		r.ResumesSeen = resumes
		r.ResumedGenerations = mergeGenerations(r.ResumedGenerations, []int{d.Generation})
	}
	count := implementationRunCount(d, r)
	otherGeneration := newGeneration || started.Generation != nil && *started.Generation != d.Generation
	if !otherGeneration && generationStarted(r, count) {
		r.Runs = count + nonGenerationRuns(r)
	} else {
		r.Runs = count - 1 + nonGenerationRuns(r)
	}
	return count
}

func classifyLocalExclusions(r *journal.Record) {
	for _, g := range r.ExcludedGenerations {
		if !slices.Contains(r.ContinuationExcludedGenerations, g) {
			r.OtherExcludedGenerations = mergeGenerations(r.OtherExcludedGenerations, []int{g})
		}
	}
}

func mergeStartEvidence(r *journal.Record, started startedExecution) {
	for _, g := range started.ExcludedGenerations {
		if !slices.Contains(started.ContinuationExcludedGenerations, g) {
			r.OtherExcludedGenerations = mergeGenerations(r.OtherExcludedGenerations, []int{g})
		}
	}
	r.OtherExcludedGenerations = mergeGenerations(r.OtherExcludedGenerations, started.OtherExcludedGenerations)
	r.StartedGenerations = mergeGenerations(r.StartedGenerations, started.StartedGenerations)
	r.ContinuationExcludedGenerations = mergeGenerations(r.ContinuationExcludedGenerations, started.ContinuationExcludedGenerations)
	for _, g := range r.StartedGenerations {
		if slices.Contains(r.ContinuationExcludedGenerations, g) {
			r.ContinuationExcludedGenerations = slices.DeleteFunc(r.ContinuationExcludedGenerations, func(v int) bool { return v == g })
			if !slices.Contains(r.OtherExcludedGenerations, g) {
				r.ExcludedGenerations = slices.DeleteFunc(r.ExcludedGenerations, func(v int) bool { return v == g })
			}
		}
	}
}

func observeStartHistory(r *journal.Record, generation int, epoch int64, host string, fresh bool) {
	if fresh && epoch == 1 && generation == 0 && host != "" {
		r.StartHistory = &journal.StartHistory{Generation: generation, Epoch: epoch, Known: true, Host: host}
		return
	}
	h := r.StartHistory
	if h == nil {
		return
	}
	continuous := epoch == h.Epoch || epoch == h.Epoch+1
	if !continuous || generation < h.Generation || host == "" || h.Host != host {
		h.Known = false
	}
	if generation > h.Generation {
		h.Known = h.Known && continuous && generation == h.Generation+1
		h.Generation = generation
	}
	h.Epoch = epoch
}

func continuationNotStarted(d Descriptor, r *journal.Record) bool {
	h := r.StartHistory
	started := field[startedExecution](d.View, "workerExecution")
	var raw struct {
		Started json.RawMessage `json:"startedGenerations"`
	}
	if json.Unmarshal(d.View["workerExecution"], &raw) != nil || len(raw.Started) == 0 || string(raw.Started) == "null" {
		return false
	}
	var recorded []int
	if json.Unmarshal(raw.Started, &recorded) != nil {
		return false
	}
	seen := make(map[int]bool, len(recorded))
	for _, g := range recorded {
		if g < 0 || g > d.Generation || seen[g] {
			return false
		}
		seen[g] = true
	}
	checkpointHistoryMatches := started.StartHistoryGeneration != nil && (*started.StartHistoryGeneration == d.Generation ||
		d.Generation > 0 && *started.StartHistoryGeneration == d.Generation-1 && r.Generation == d.Generation-1 &&
			started.Generation != nil && *started.Generation == d.Generation-1)
	return h != nil && h.Known && h.Generation == d.Generation && h.Epoch == r.Epoch &&
		started.Generation != nil && *started.Generation >= 0 && *started.Generation <= d.Generation &&
		checkpointHistoryMatches &&
		!slices.Contains(r.StartedGenerations, d.Generation) && !slices.Contains(started.StartedGenerations, d.Generation)
}

func markImplementationStarted(d Descriptor, r *journal.Record) {
	r.StartedGenerations = mergeGenerations(r.StartedGenerations, []int{d.Generation})
	if slices.Contains(r.ContinuationExcludedGenerations, d.Generation) {
		r.ContinuationExcludedGenerations = slices.DeleteFunc(r.ContinuationExcludedGenerations, func(g int) bool { return g == d.Generation })
		if !slices.Contains(r.OtherExcludedGenerations, d.Generation) {
			r.ExcludedGenerations = slices.DeleteFunc(r.ExcludedGenerations, func(g int) bool { return g == d.Generation })
		}
	}
}

// A later lease on another PC may have started this same generation. Preserve
// earlier generations' settled exclusions, but count the current one unless
// the local uninterrupted history still proves it has not started.
func clearUncertainCurrentExclusion(d Descriptor, r *journal.Record) {
	if !slices.Contains(r.ContinuationExcludedGenerations, d.Generation) || continuationNotStarted(d, r) {
		return
	}
	before := implementationRunCount(d, r)
	r.ContinuationExcludedGenerations = slices.DeleteFunc(r.ContinuationExcludedGenerations, func(g int) bool { return g == d.Generation })
	if !slices.Contains(r.OtherExcludedGenerations, d.Generation) {
		r.ExcludedGenerations = slices.DeleteFunc(r.ExcludedGenerations, func(g int) bool { return g == d.Generation })
	}
	if r.Generation == d.Generation {
		r.Runs += implementationRunCount(d, r) - before
	}
}

func generationStarted(r *journal.Record, count int) bool {
	return r.Runs-nonGenerationRuns(r) >= count
}

// excludeGeneration は今の世代を、実装のせいでないものとして回数から外す。
func excludeGeneration(d Descriptor, r *journal.Record) {
	r.OtherExcludedGenerations = mergeGenerations(r.OtherExcludedGenerations, []int{d.Generation})
	r.ContinuationExcludedGenerations = slices.DeleteFunc(r.ContinuationExcludedGenerations, func(g int) bool { return g == d.Generation })
	r.ExcludedGenerations = mergeGenerations(r.ExcludedGenerations, []int{d.Generation})
	r.Runs = implementationRunCount(d, r) + nonGenerationRuns(r)
}

// fixedTestNotAttributable は固定テストの失敗が実装のせいでないと証拠から言えるかを返す。環境の問題と、
// 保存済みの基点の証拠で同じ失敗と分かるものだけを認める。受付へ報告する fixedTestCause と同じ判定を使う。
func (e *Executor) fixedTestNotAttributable(r *journal.Record, test TestResult) bool {
	if test.Passed {
		return false
	}
	if test.Unavailable || test.Observation["environmentFailure"] != nil {
		return true
	}
	if test.Observation == nil {
		return false
	}
	// finish と同じく、保存形のJSONから読み直した証拠で判定する。
	var saved TestResult
	if json.Unmarshal(encode(test), &saved) != nil || saved.Observation == nil {
		return false
	}
	kind, _ := e.classifyTest(r.Repo, saved.Observation)["kind"].(string)
	return kind == "environment" || kind == "baseline"
}

func mergeGenerations(current, more []int) []int {
	for _, g := range more {
		if g >= 0 && !slices.Contains(current, g) {
			current = append(current, g)
		}
	}
	slices.Sort(current)
	return current
}

package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/atomicfile"
	"issue-desk-worker/internal/cause"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// GenerationTrigger records why the implementation of Generation started. Usage rows
// of the implementation and conflict steps carry it while the generation is current.
// An empty Trigger means none (the first generation).
type GenerationTrigger struct {
	Generation   int      `json:"generation"`
	Trigger      string   `json:"trigger"`
	FindingKinds []string `json:"findingKinds,omitempty"`
}

// ReportCompletion identifies one received implementation report. Attempted is saved
// before dispatch, so an interrupted or ambiguous call can never be retried.
type ReportCompletion struct {
	ID           string          `json:"id"`
	Generation   int             `json:"generation"`
	Stage        string          `json:"stage"`
	Original     json.RawMessage `json:"original"`
	Attempted    bool            `json:"attempted"`
	Validated    bool            `json:"validated"`
	EditsStarted bool            `json:"editsStarted,omitempty"`
	EditsApplied bool            `json:"editsApplied,omitempty"`
	Error        string          `json:"error,omitempty"`
	Provider     string          `json:"provider"`
	Session      string          `json:"session"`
}

type AppliedEdits struct {
	Head        string    `json:"head"`
	RequestHash string    `json:"requestHash"`
	CreatedAt   time.Time `json:"createdAt"`
	WorkerID    string    `json:"workerId"`
}

type ReviewUsageRecovery struct {
	Head      string `json:"head"`
	Base      string `json:"base"`
	PatchHash string `json:"patchHash"`
}

type Record struct {
	StoppedBeforeStart              bool                       `json:"stoppedBeforeStart,omitempty"`
	NoDiffClose                     *NoDiffClose               `json:"noDiffClose,omitempty"`
	IntakeMergeRecovery             bool                       `json:"intakeMergeRecovery,omitempty"`
	ExecutionReportHead             string                     `json:"executionReportHead,omitempty"`
	ExecutionAdmissionView          map[string]json.RawMessage `json:"executionAdmissionView,omitempty"`
	ExecutionReportPending          bool                       `json:"executionReportPending,omitempty"`
	ExecutionReportResult           json.RawMessage            `json:"executionReportResult,omitempty"`
	ExecutionTier                   string                     `json:"executionTier,omitempty"`
	ExecutionPlan                   json.RawMessage            `json:"executionPlan,omitempty"`
	ExecutionPlanSHA                string                     `json:"executionPlanSHA,omitempty"`
	ExecutionReport                 json.RawMessage            `json:"executionReport,omitempty"`
	AppliedEdits                    *AppliedEdits              `json:"appliedEdits,omitempty"`
	EditRequestedAt                 time.Time                  `json:"editRequestedAt,omitempty"`
	EditRequestHash                 string                     `json:"editRequestHash,omitempty"`
	EditWorkerID                    string                     `json:"editWorkerId,omitempty"`
	StartHistory                    *StartHistory              `json:"startHistory,omitempty"`
	StartedGenerations              []int                      `json:"startedGenerations,omitempty"`
	ContinuationExcludedGenerations []int                      `json:"continuationExcludedGenerations,omitempty"`
	OtherExcludedGenerations        []int                      `json:"otherExcludedGenerations,omitempty"`
	IncompleteRun                   *IncompleteRun             `json:"incompleteRun,omitempty"`
	TimingPending                   bool                       `json:"stageTimingPending,omitempty"`
	Timing                          *StageTiming               `json:"stageTiming,omitempty"`
	timingRunning                   bool
	loaded                          bool
	freshlySaved                    bool
	ReviewBaseline                  *ReviewBaseline      `json:"reviewBaseline,omitempty"`
	ReviewUsageRecovery             *ReviewUsageRecovery `json:"reviewUsageRecovery,omitempty"`
	CarriedHeldReview               *CarriedHeldReview   `json:"carriedHeldReview,omitempty"`
	// Add rounds only through AddReviewRound so ReviewCounts stays accurate.
	ReviewRounds                 []ReviewRound        `json:"reviewRounds,omitempty"`
	ReviewCounts                 *ReviewCounts        `json:"reviewCounts,omitempty"`
	ReviewSkipped                bool                 `json:"reviewSkipped,omitempty"`
	ScopeNarrowed                int                  `json:"scopeNarrowed,omitempty"`
	ScopeNarrowFailed            int                  `json:"scopeNarrowFailed,omitempty"`
	NotesAttempted               []string             `json:"notesAttempted,omitempty"`
	RetryBaseGeneration          *int                 `json:"retryBaseGeneration,omitempty"`
	DiscardedContinuationSources []ContinuationSource `json:"discardedContinuationSources,omitempty"`
	ReportCompletion             *ReportCompletion    `json:"reportCompletion,omitempty"`
	AntigravityDenied            []ProviderDenied     `json:"antigravityDenied,omitempty"`
	AntigravityDenialHold        *ProviderDenialHold  `json:"antigravityDenialHold,omitempty"`
	AntigravityStrikes           []ProviderFailure    `json:"antigravityStrikes,omitempty"`
	ProviderSwitch               *ProviderSwitch      `json:"providerSwitch,omitempty"`
	AntigravityInputs            string               `json:"antigravityInputs,omitempty"`
	AdmissionID                  string               `json:"admissionId,omitempty"`
	AdmissionRun                 int                  `json:"admissionRun,omitempty"`
	AdmissionGeneration          int                  `json:"admissionGeneration,omitempty"`
	AdmissionRecall              bool                 `json:"admissionRecall,omitempty"`
	FinalReported                bool                 `json:"finalReported"`
	Epoch                        int64                `json:"epoch"`
	ID                           int64                `json:"id"`
	Repo                         string               `json:"repo"`
	Issue                        int64                `json:"issue"`
	Generation                   int                  `json:"generation"`
	Stage                        string               `json:"stage"`
	Branch                       string               `json:"branch"`
	// Nil is an older, uncaptured journal. Empty records that this job had no
	// prior branch; persisting a hold must not manufacture continuation evidence.
	ContinuationBranch   *string            `json:"continuationBranch,omitempty"`
	Tree                 string             `json:"tree"`
	TargetBase           string             `json:"targetBase,omitempty"`
	Base                 string             `json:"base"`
	Head                 string             `json:"head"`
	PRCreationAttempted  bool               `json:"prCreationAttempted,omitempty"`
	PR                   int64              `json:"pr"`
	Provider             string             `json:"provider"`
	Session              string             `json:"session"`
	Runs                 int                `json:"runs"`                           // Started generations by the intake's formula, plus the two counts below.
	ReportCompletionRuns int                `json:"reportCompletionRuns,omitempty"` // Included in Runs for limits; excluded when classifying automatic starts.
	UnansweredRuns       int                `json:"unansweredRuns,omitempty"`       // Unanswered starts beyond the interrupted allowance; counted like the above.
	ReviewReworkRuns     int                `json:"reviewReworkRuns,omitempty"`     // Reimplementations after an independent review held the change; counted like the above.
	ExcludedGenerations  []int              `json:"excludedGenerations,omitempty"`  // Generations whose failure was not the implementation's fault.
	ResumedGenerations   []int              `json:"resumedGenerations,omitempty"`   // Generations the intake counted in implementationResumes.
	ResumesSeen          int                `json:"resumesSeen,omitempty"`
	OpenRun              int                `json:"openRun,omitempty"` // The started run that has not returned an answer yet.
	OpenRunGeneration    int                `json:"openRunGeneration,omitempty"`
	OpenRunEpoch         int64              `json:"openRunEpoch,omitempty"`
	RejectedStart        *RejectedStart     `json:"rejectedStart,omitempty"`
	InterruptedRuns      int                `json:"interruptedRuns,omitempty"` // Starts taken back because no answer ever came.
	ReviewHolds          int                `json:"reviewHolds"`
	ReviewHead           string             `json:"reviewHead"`
	Result               json.RawMessage    `json:"implementationResult"`
	Review               json.RawMessage    `json:"review"`
	FollowupProposals    []FollowupProposal `json:"followupProposals,omitempty"`
	PreviousResult       json.RawMessage    `json:"previousImplementationResult,omitempty"`
	PreviousReview       json.RawMessage    `json:"previousReview,omitempty"`
	FixedTestFailures    int                `json:"fixedTestFailures,omitempty"`
	Test                 json.RawMessage    `json:"test"`
	ControllerAllowed    []string           `json:"controllerAllowed"`
	ControllerChanges    []json.RawMessage  `json:"controllerChanges,omitempty"`
	FinalState           json.RawMessage    `json:"finalState"`
	FinalReportEpoch     int64              `json:"finalReportEpoch,omitempty"`
	// GenerationTrigger is why the current implementation generation started (see execution.generation_trigger.go).
	GenerationTrigger *GenerationTrigger `json:"generationTrigger,omitempty"`
	// UsageTrigger is the trigger already sent for the usage rows' generation (Generation is the row generation).
	UsageTrigger  *GenerationTrigger `json:"usageTrigger,omitempty"`
	ProviderHold  json.RawMessage    `json:"providerHold,omitempty"`
	Usage         []json.RawMessage  `json:"usage"`
	UsageSent     int                `json:"usageSent"`
	Limits        []json.RawMessage  `json:"limits,omitempty"`
	WeightedUsage float64            `json:"weightedUsage"`
	UpdatedAt     time.Time          `json:"updatedAt"`
}

// StartHistory is local evidence; it is never reconstructed from a remote checkpoint.
type StartHistory struct {
	Generation int    `json:"generation"`
	Epoch      int64  `json:"epoch"`
	Known      bool   `json:"known"`
	Host       string `json:"host"`
}

// IncompleteRun belongs to one implementation run, including its test repairs.
// A missing record in an older journal does not authorize a continuation.
type IncompleteRun struct {
	Generation int       `json:"generation"`
	Run        int       `json:"run"`
	StartedAt  time.Time `json:"startedAt"`
	Attempted  bool      `json:"attempted"`
}
type Store struct {
	Root  string
	mu    sync.Mutex
	lock  *os.File
	index map[string]indexEntry
	// Corrupt is told about each corrupt journal file that List skipped, once
	// per version of the file. Nil logs to the standard logger. It is called
	// with the store locked, so it must not call Store methods.
	Corrupt  func(*CorruptError)
	reported map[string]indexEntry
}

// CorruptError is a journal file whose content cannot be decoded as the job it
// is named after. The file is left in place for investigation.
type CorruptError struct {
	ID    int64
	Path  string
	Cause string
}

func (e *CorruptError) Error() string {
	return fmt.Sprintf("保存済みの仕事%d（%s）が不正です: %s", e.ID, e.Path, e.Cause)
}

func decodeRecord(id int64, path string, data []byte, r *Record) error {
	if err := json.Unmarshal(data, r); err != nil {
		return &CorruptError{ID: id, Path: path, Cause: "JSONとして読めません（" + err.Error() + "）"}
	}
	if r.ID != id {
		return &CorruptError{ID: id, Path: path, Cause: fmt.Sprintf("中の仕事番号が%dです", r.ID)}
	}
	return nil
}

func readError(id int64, path string, err error) error {
	return fmt.Errorf("保存済みの仕事%d（%s）を読めません: %w", id, path, err)
}

// skipCorrupt reports err once per version of the file. s.mu must be held.
func (s *Store) skipCorrupt(err *CorruptError) {
	if info, statErr := os.Stat(err.Path); statErr == nil {
		stat, _ := info.Sys().(*syscall.Stat_t)
		seen, done := s.reported[err.Path]
		if done && sameIndexFile(seen, info) {
			return
		}
		if s.reported == nil {
			s.reported = make(map[string]indexEntry)
		}
		if stat != nil {
			s.reported[err.Path] = indexEntry{info: info, change: statChangeTime(stat)}
		}
	}
	if s.Corrupt != nil {
		s.Corrupt(err)
		return
	}
	log.Printf("壊れた仕事の記録を飛ばしました（消していません）: %v", err)
}

func Open(root string) (*Store, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("runtimeDirは絶対パスにしてください")
	}
	for _, dir := range []string{root, filepath.Join(root, "state"), filepath.Join(root, "control"), filepath.Join(root, "control", "results"), filepath.Join(root, "workspaces")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("実行状態フォルダーを作れません: %w", err)
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, cause.New("実行状態フォルダーが通常のディレクトリではありません", err)
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return nil, fmt.Errorf("実行状態フォルダーを保護できません: %w", err)
		}
	}
	file, err := os.OpenFile(filepath.Join(root, "state", "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("同じ実行状態を使うWorkerが既に動いています")
	}
	return &Store{Root: root, lock: file}, nil
}
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	err := s.lock.Close()
	s.lock = nil
	return err
}
func (s *Store) file(id int64) string {
	return filepath.Join(s.Root, "state", fmt.Sprintf("job-%d.json", id))
}
func (s *Store) Load(id int64) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(id)
}

// load reads one journal file. s.mu must be held.
func (s *Store) load(id int64) (*Record, error) {
	path := s.file(id)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, readError(id, path, err)
	}
	var r Record
	if err := decodeRecord(id, path, data, &r); err != nil {
		return nil, err
	}
	r.loaded = true
	return &r, nil
}

// LoadAfterSave carries only in-memory provenance from a newly created record
// through the immediate reload in the same execution. It is never persisted.
func (s *Store) LoadAfterSave(original *Record) (*Record, error) {
	r, err := s.Load(original.ID)
	if err == nil && r != nil && original.freshlySaved {
		r.freshlySaved = true
	}
	return r, err
}

// Save updates r in place, including truncating ReviewRounds to the newest entries.
func (s *Store) Save(r *Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ID < 1 {
		return errors.New("保存するジョブIDが不正です")
	}
	r.EnsureReviewCounts()
	r.ReviewRounds = LimitReviewRounds(r.ReviewRounds)
	r.UpdatedAt = time.Now().UTC()
	r.observeStage(r.UpdatedAt)
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := atomicfile.Write(s.file(r.ID), ".job-*.tmp", 0600, true, data, nil); err != nil {
		return err
	}
	if !r.loaded && r.Timing == nil {
		r.freshlySaved = true
	}
	return nil
}

// List returns every readable job. A corrupt file is reported and skipped, so
// one broken record does not stop the others; Load of that ID still fails.
func (s *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "state"))
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, entry := range entries {
		var id int64
		if _, err := fmt.Sscanf(entry.Name(), "job-%d.json", &id); err != nil {
			continue
		}
		s.mu.Lock()
		record, err := s.load(id)
		var corrupt *CorruptError
		if errors.As(err, &corrupt) {
			s.skipCorrupt(corrupt)
			err = nil
		}
		s.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if record != nil {
			records = append(records, *record)
		}
	}
	return records, nil
}

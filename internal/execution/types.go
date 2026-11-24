package execution

import (
	"encoding/json"
	"issue-desk-worker/internal/jobspec"
	"strings"

	"issue-desk-worker/internal/journal"
)

// Descriptor is the intake's job descriptor with how this Executor is
// running it right now. The modes are never read from or sent to the intake.
type Descriptor struct {
	jobspec.Descriptor
	// reportOnly runs the provider read-only to complete a report.
	reportOnly bool
	// conflictResolution asks the provider to resolve a merge conflict.
	conflictResolution bool
	// finalApprovalFixedTest reruns only the fixed test for final approval.
	finalApprovalFixedTest bool
}
type Implementation struct {
	Status              string          `json:"status"`
	Summary             string          `json:"summary"`
	Questions           []string        `json:"questions"`
	Unverified          []string        `json:"unverified"`
	Incomplete          json.RawMessage `json:"incomplete"`
	PrerequisiteMissing json.RawMessage `json:"prerequisiteMissing"`
}
type previousFindingLink struct {
	Index   int    `json:"index"`
	Status  string `json:"status"`
	Current *int   `json:"current"`
}

type reviewInterrupt struct {
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

type Review struct {
	PreviousFindings        []previousFindingLink  `json:"previousFindings,omitempty"`
	Decision                string                 `json:"decision"`
	Summary                 string                 `json:"summary"`
	Breaking                bool                   `json:"breaking"`
	TooLarge                bool                   `json:"tooLarge"`
	Followups               []string               `json:"followups,omitempty"`
	Findings                []string               `json:"findings"`
	Kinds                   []string               `json:"findingKinds"`
	Severities              []string               `json:"findingSeverities"`
	Uncertainties           []string               `json:"uncertainties,omitempty"`
	UserJudgments           []string               `json:"userJudgments"`
	ResolvableUncertainties []string               `json:"resolvableUncertainties"`
	Improvements            []string               `json:"improvements,omitempty"`
	Interrupts              []reviewInterrupt      `json:"interrupts,omitempty"`
	RecordOnly              []string               `json:"recordOnly,omitempty"`
	FindingSpecs            []reviewFindingSpec    `json:"findingSpecs,omitempty"`
	ImprovementSpecs        []followupProposalItem `json:"improvementSpecs,omitempty"`
	// Worker-owned metadata, carried by the existing saved-review JSON.
	ReviewedHead             string                   `json:"reviewedHead,omitempty"`
	ReviewedGeneration       *int                     `json:"reviewedGeneration,omitempty"`
	ReviewedEpoch            int64                    `json:"reviewedEpoch,omitempty"`
	DeferredReviewItems      []followupProposalItem   `json:"deferredReviewItems,omitempty"`
	DeferredReviewDeliveries []deferredReviewDelivery `json:"deferredReviewDeliveries,omitempty"`
	ReviewComparisonBlocked  bool                     `json:"reviewComparisonBlocked,omitempty"`
}

type TestResult struct {
	Diagnostic       *TestDiagnostic       `json:"diagnostic,omitempty"`
	AppliedEdits     *journal.AppliedEdits `json:"appliedEdits,omitempty"`
	countableFailure bool
	evidenceMissing  bool
	evidenceReason   bool
	environmentHold  *environmentHold
	// accessDenied is true only when this run itself was refused access; a host hold alone does not set it.
	accessDenied bool
	Unavailable  bool           `json:"unavailable,omitempty"`
	Passed       bool           `json:"passed"`
	Command      []string       `json:"command"`
	Detail       string         `json:"detail"`
	ElapsedMS    int64          `json:"elapsedMs"`
	Observation  map[string]any `json:"causeObservation,omitempty"`
}

func hasJSON(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value != "" && value != "null"
}
func encode(v any) json.RawMessage { data, _ := json.Marshal(v); return data }
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}
func field[T any](view map[string]json.RawMessage, name string) T {
	var out T
	_ = json.Unmarshal(view[name], &out)
	return out
}

type instructionContent struct {
	Text    string `json:"text"`
	Context string `json:"context"`
}

func structuredInstructions(d Descriptor) []instructionContent {
	var list []instructionContent
	_ = json.Unmarshal(d.View["instructions"], &list)
	return list
}

// Keep the existing empty value so reports and review identities without instructions stay compatible.
func instructionData(d Descriptor) any {
	list := structuredInstructions(d)
	for _, entry := range list {
		if entry.Text != "" || entry.Context != "" {
			return list
		}
	}
	return instructions(d)
}

func instructions(d Descriptor) string {
	var parts []string
	for _, entry := range structuredInstructions(d) {
		parts = append(parts, entry.Text+entry.Context)
	}
	return strings.Join(parts, "\n")
}

package runtime

import (
	"errors"
	"log"
	"strconv"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
)

// The executor owns the allowlist; this interface avoids coupling runtime to it.
type executionDiagnostic interface {
	ExecutionStage() string
	SafeExecutionReason() string
}

func executionErrorDetail(err error) map[string]string {
	detail := map[string]string{"kind": jobErrorKind(err)}
	var diagnostic executionDiagnostic
	if errors.As(err, &diagnostic) {
		if stage := diagnostic.ExecutionStage(); stage != "" {
			detail["stage"] = stage
		}
		if reason := diagnostic.SafeExecutionReason(); reason != "" {
			detail["reason"] = reason
		}
	}
	var api *gitea.Error
	if errors.As(err, &api) && api != nil && api.Status >= 100 && api.Status <= 599 {
		detail["giteaHttpStatus"] = strconv.Itoa(api.Status)
	}
	if detail["kind"] == jobErrorAPIRejected {
		var refusal *intake.APIError
		if errors.As(err, &refusal) && refusal != nil {
			if refusal.Status >= 100 && refusal.Status <= 599 {
				detail["intakeHttpStatus"] = strconv.Itoa(refusal.Status)
			}
			if safeIntakeCode(refusal.Code) {
				detail["intakeCode"] = refusal.Code
			}
		}
	}
	return detail
}

// These fixed values originate in intake's response normalization and contract.
func safeIntakeCode(code string) bool {
	switch code {
	case "request_failed", "turn_lost", "contract_mismatch":
		return true
	default:
		return false
	}
}

func logExecutionError(id int64, detail map[string]string) {
	// Quote optional values so whitespace and punctuation cannot split log fields.
	status := ""
	if n, err := strconv.Atoi(detail["giteaHttpStatus"]); err == nil && n >= 100 && n <= 599 && strconv.Itoa(n) == detail["giteaHttpStatus"] {
		status = " gitea_http_status=" + strconv.Itoa(n)
	}
	if detail["kind"] == jobErrorAPIRejected {
		if n, err := strconv.Atoi(detail["intakeHttpStatus"]); err == nil && n >= 100 && n <= 599 && strconv.Itoa(n) == detail["intakeHttpStatus"] {
			status += " intake_http_status=" + strconv.Quote(strconv.Itoa(n))
		}
		if safeIntakeCode(detail["intakeCode"]) {
			status += " intake_code=" + strconv.Quote(detail["intakeCode"])
		}
	}
	switch {
	case detail["stage"] != "" && detail["reason"] != "":
		log.Printf("scheduler job=%d event=execution_error reason=%s stage=%q execution_reason=%q%s", id, detail["kind"], detail["stage"], detail["reason"], status)
	case detail["stage"] != "":
		log.Printf("scheduler job=%d event=execution_error reason=%s stage=%q%s", id, detail["kind"], detail["stage"], status)
	case detail["reason"] != "":
		log.Printf("scheduler job=%d event=execution_error reason=%s execution_reason=%q%s", id, detail["kind"], detail["reason"], status)
	default:
		log.Printf("scheduler job=%d event=execution_error reason=%s%s", id, detail["kind"], status)
	}
}

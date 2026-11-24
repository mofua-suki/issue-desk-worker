package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/providers"
)

// Report only a safe classification. The intake merges top-level view keys;
// this deliberately leaves checkpoints, results and usage records untouched.
func (r Runner) reportExecutionError(ctx context.Context, guard *leaseGuard, id int64, cause error, at time.Time) {
	lease, err := guard.current()
	if err != nil || ctx.Err() != nil {
		return
	}
	detail := executionErrorDetail(cause)
	detail["at"] = at.UTC().Format(time.RFC3339)
	view, err := json.Marshal(map[string]any{"lastExecutionError": detail})
	if err != nil {
		return
	}
	reportCtx, cancel := context.WithTimeout(ctx, min(30*time.Second, lease.ExpiresAt.Sub(r.Now())))
	defer cancel()
	if reportCtx.Err() != nil {
		return
	}
	if _, err := guard.current(); err != nil {
		return
	}
	_ = r.API.State(reportCtx, id, lease.Epoch, intake.State{Status: "running", View: view})
}

// The CLI prints returned errors. Preserve errors.Is/As for callers without
// exposing a server-supplied API code or another uninspected error string.
type schedulerStopError struct {
	cause  error
	id     int64
	reason string
}

func (e *schedulerStopError) Error() string {
	return fmt.Sprintf("scheduler job=%d reason=%s: %s", e.id, e.reason, e.message())
}
func (e *schedulerStopError) Unwrap() error { return e.cause }

func (e *schedulerStopError) message() string {
	var api *intake.APIError
	var transport *intake.TransportError
	switch {
	case errors.As(e.cause, &api):
		return fmt.Sprintf("受付APIがHTTP %dを返しました", api.Status)
	case errors.As(e.cause, &transport):
		return transport.Error() // This type contains only a boolean, no remote text.
	case errors.Is(e.cause, context.DeadlineExceeded):
		return "受付APIの要求が時間切れになりました"
	}
	// Preserve the original local Japanese messages, never arbitrary cause text.
	// The intake adds only the request's method and path template or the
	// failure's kind after a message, so a message with such a suffix is kept.
	text := e.cause.Error()
	for _, message := range []string{
		"仕事のない応答にleaseがあります",
		"仕事のIDまたはleaseが不正です",
		"配布の仕事とleaseのepochが一致しません",
		"実行中の仕事が重複して配布されました",
		"受付の配布が設定のリポジトリ枠と一致しません",
		"受付APIの証明書を検証できません",
		"受付APIのJSON応答が不正です",
		"受付APIの転送を拒否しました",
		"受付APIの応答が上限を超えました",
	} {
		if text == message || strings.HasPrefix(text, message) && intakeSuffix.MatchString(strings.TrimPrefix(text, message)) {
			return text
		}
	}
	return "受付の仕事取得または配布の確認に失敗しました"
}

// intakeSuffix is what the intake adds after a fixed message: the method and
// path template of the request, then the kind of the failure. Anything else
// is not known to be free of values and is not kept.
var intakeSuffix = regexp.MustCompile(`^(（[A-Z]+ /[A-Za-z0-9/{}._-]*）)?(（[^（）:/\\]{1,40}）)?$`)

// These are the complete kind values sent to intake in lastExecutionError.
const (
	jobErrorLeaseExpired       = "lease_expired"
	jobErrorLeaseLost          = "lease_lost"
	jobErrorCanceled           = "canceled"
	jobErrorTimeout            = "timeout"
	jobErrorAPIRejected        = "api_rejected"
	jobErrorTransportFailure   = "transport_failure"
	jobErrorGiteaAPI           = "gitea_api_error"
	jobErrorFilesystem         = "filesystem_error"
	jobErrorProviderReport     = "provider_report_error"
	jobErrorProviderUsageLimit = "provider_usage_limit"
	jobErrorExecution          = "execution_error"
)

// anyErrorBranch visits single and joined unwrap paths. A predicate using
// errors.As must be retried on each branch when it also checks matched values:
// the first match in a subtree may not satisfy that additional condition.
func anyErrorBranch(err error, matches func(error) bool) bool {
	if err == nil {
		return false
	}
	if matches(err) {
		return true
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		for _, child := range e.Unwrap() {
			if anyErrorBranch(child, matches) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return anyErrorBranch(e.Unwrap(), matches)
	}
	return false
}

func hasLeaseLost(err error) bool {
	return anyErrorBranch(err, func(branch error) bool {
		var api *intake.APIError
		return errors.As(branch, &api) && api.LeaseLost()
	})
}

// Only these locally selected labels may reach logs.
// API codes, URLs and arbitrary executor error strings are never interpolated.
// Type-only errors.As checks already search all branches; if a classification
// gains a condition on the matched value, use anyErrorBranch as for lease loss
// and test joined errors in both branch orders, including a nonmatching value
// of the same type. Review branch-order tests when called intake predicates change.
func jobErrorKind(err error) string {
	var api *intake.APIError
	var gitAPI *gitea.Error
	var pathErr *os.PathError
	var linkErr *os.LinkError
	var syscallErr *os.SyscallError
	var netErr *net.OpError
	var reportErr *providers.ReportOnlyError
	var limit *providers.UsageLimit
	switch {
	case errors.Is(err, ErrLeaseExpired):
		return jobErrorLeaseExpired
	case hasLeaseLost(err):
		return jobErrorLeaseLost
	case errors.Is(err, context.Canceled):
		return jobErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return jobErrorTimeout
	case errors.As(err, &api):
		return jobErrorAPIRejected
	case errors.As(err, &reportErr):
		return jobErrorProviderReport
	case errors.As(err, &limit):
		return jobErrorProviderUsageLimit
	case intake.TemporaryFailureReason(err) != "":
		return jobErrorTransportFailure
	case errors.As(err, &netErr):
		return jobErrorTransportFailure
	case errors.As(err, &gitAPI):
		return jobErrorGiteaAPI
	case errors.As(err, &pathErr) || errors.As(err, &linkErr) || errors.As(err, &syscallErr):
		return jobErrorFilesystem
	default:
		return jobErrorExecution
	}
}

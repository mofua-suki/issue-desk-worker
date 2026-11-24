package runtime

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"issue-desk-worker/internal/execution"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/providers"
)

func TestExecutionErrorDetailKeyContract(t *testing.T) {
	err := errors.Join(&execution.ExecutionError{Err: errors.New("公開PRのリポジトリ・枝・既定ブランチ・headが一致しません"), Stage: "publish"}, &gitea.Error{Status: 503})
	detail := executionErrorDetail(err)
	want := map[string]string{"kind": "gitea_api_error", "stage": "publish", "reason": "公開PRのリポジトリ・枝・既定ブランチ・headが一致しません", "giteaHttpStatus": "503"}
	if !reflect.DeepEqual(detail, want) {
		t.Fatalf("detail = %#v, want %#v", detail, want)
	}
}

func TestHasLeaseLostNestedJoin(t *testing.T) {
	ordinary := &intake.APIError{Status: 400}
	lost := &intake.APIError{Status: 409, Code: "lease_lost"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"later branch", fmt.Errorf("outer: %w", errors.Join(ordinary, lost)), true},
		{"first branch", fmt.Errorf("outer: %w", errors.Join(lost, ordinary)), true},
		{"no loss", fmt.Errorf("outer: %w", errors.Join(ordinary, errors.New("other"))), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasLeaseLost(tc.err); got != tc.want {
				t.Fatalf("hasLeaseLost = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestJobErrorKindNestedReportAndAPIPriority(t *testing.T) {
	ordinary := &intake.APIError{Status: 400}
	lost := &intake.APIError{Status: 409, Code: "lease_lost"}
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"api inside report", &providers.ReportOnlyError{Err: ordinary}, jobErrorAPIRejected},
		{"report first", fmt.Errorf("outer: %w", errors.Join(&providers.ReportOnlyError{Err: ordinary}, lost)), jobErrorLeaseLost},
		{"loss first", fmt.Errorf("outer: %w", errors.Join(lost, &providers.ReportOnlyError{Err: ordinary})), jobErrorLeaseLost},
		{"report in later branch", errors.Join(errors.New("other"), &providers.ReportOnlyError{Err: errors.New("report")}), jobErrorProviderReport},
		{"usage in later branch", errors.Join(errors.New("other"), &providers.UsageLimit{}), jobErrorProviderUsageLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobErrorKind(tc.err); got != tc.want {
				t.Fatalf("jobErrorKind = %q, want %q", got, tc.want)
			}
		})
	}
}

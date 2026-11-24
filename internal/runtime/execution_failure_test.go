package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"issue-desk-worker/internal/execution"
	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
)

func TestExecutionErrorDetailGiteaStatus(t *testing.T) {
	var nilGitea *gitea.Error
	for _, tc := range []struct {
		name                string
		err                 error
		status, kind, stage string
	}{
		{"direct lower", &gitea.Error{Status: 100}, "100", "gitea_api_error", ""},
		{"wrapped upper", fmt.Errorf("wrap: %w", &gitea.Error{Status: 599}), "599", "gitea_api_error", ""},
		{"join first", errors.Join(&gitea.Error{Status: 503}, &gitea.Error{Status: 404}), "503", "gitea_api_error", ""},
		{"execution", &execution.ExecutionError{Err: &gitea.Error{Status: 418}, Stage: "publish"}, "418", "gitea_api_error", "publish"},
		{"below", &gitea.Error{Status: 99}, "", "gitea_api_error", ""},
		{"above", &gitea.Error{Status: 600}, "", "gitea_api_error", ""},
		{"zero", &gitea.Error{}, "", "gitea_api_error", ""},
		{"negative", &gitea.Error{Status: -1}, "", "gitea_api_error", ""},
		{"nil", nil, "", "execution_error", ""},
		{"typed nil first", errors.Join(nilGitea, &gitea.Error{Status: 503}), "", "gitea_api_error", ""},
		{"invalid first", errors.Join(&gitea.Error{Status: 99}, &gitea.Error{Status: 503}), "", "gitea_api_error", ""},
		{"other", errors.New("other"), "", "execution_error", ""},
		{"lease priority", errors.Join(&intake.APIError{Status: 409, Code: "lease_lost"}, &gitea.Error{Status: 503}), "503", "lease_lost", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail := executionErrorDetail(tc.err)
			if detail["kind"] != tc.kind || detail["stage"] != tc.stage || detail["giteaHttpStatus"] != tc.status {
				t.Fatalf("detail = %#v", detail)
			}
			if _, ok := detail["giteaHttpStatus"]; ok != (tc.status != "") {
				t.Fatalf("status presence: %#v", detail)
			}
		})
	}
}

func TestIntakeErrorAttributes(t *testing.T) {
	for _, tc := range []struct {
		name               string
		err                error
		kind, status, code string
	}{
		{"lower", &intake.APIError{Status: 100, Code: "turn_lost"}, "api_rejected", "100", "turn_lost"},
		{"upper", &intake.APIError{Status: 599, Code: "contract_mismatch"}, "api_rejected", "599", "contract_mismatch"},
		{"below", &intake.APIError{Status: 99, Code: "request_failed"}, "api_rejected", "", "request_failed"},
		{"above", &intake.APIError{Status: 600, Code: "request_failed"}, "api_rejected", "", "request_failed"},
		{"empty", &intake.APIError{Status: 400}, "api_rejected", "400", ""},
		{"invalid", &intake.APIError{Status: 400, Code: "CONTRACT_MISMATCH"}, "api_rejected", "400", ""},
		{"long", &intake.APIError{Status: 400, Code: strings.Repeat("a", 65)}, "api_rejected", "400", ""},
		{"secret", &intake.APIError{Status: 400, Code: "secret_token_123"}, "api_rejected", "400", ""},
		{"first invalid", errors.Join(&intake.APIError{Status: 99, Code: "secret_token_123"}, &intake.APIError{Status: 409, Code: "request_failed"}), "api_rejected", "", ""},
		{"canceled priority", errors.Join(context.Canceled, &intake.APIError{Status: 409, Code: "request_failed"}), "canceled", "", ""},
		{"lease priority", errors.Join(&intake.APIError{Status: 400, Code: "request_failed"}, &intake.APIError{Status: 409, Code: "lease_lost"}), "lease_lost", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detail := executionErrorDetail(tc.err)
			if detail["kind"] != tc.kind || detail["intakeHttpStatus"] != tc.status || detail["intakeCode"] != tc.code {
				t.Fatalf("detail = %#v", detail)
			}
			for key, value := range map[string]string{"intakeHttpStatus": tc.status, "intakeCode": tc.code} {
				if _, ok := detail[key]; ok != (value != "") {
					t.Fatalf("unexpected presence of %s: %#v", key, detail)
				}
			}
		})
	}
}

func TestIntakeErrorReportAndLogPrivacy(t *testing.T) {
	for _, code := range []string{"request_failed", "secret_token_123"} {
		t.Run(code, func(t *testing.T) {
			now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			var body []byte
			api := leaseClient(t, func(req *http.Request) (*http.Response, error) {
				body, _ = io.ReadAll(req.Body)
				return leaseResponse(204, "")
			})
			cause := &intake.APIError{Status: 409, Code: code}
			wrapped := &execution.ExecutionError{Err: errors.Join(errors.New("secret-wrapper https://example.invalid Bearer token"), cause), Stage: "publish"}
			runner := Runner{API: api, Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
				return intake.State{}, wrapped
			}}, RenewInterval: time.Hour, Now: func() time.Time { return now }}
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			if err := runner.execute(context.Background(), assignment(now)); !errors.Is(err, cause) {
				t.Fatalf("error = %v", err)
			}
			var payload struct {
				View struct {
					Detail map[string]string `json:"lastExecutionError"`
				} `json:"view"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			detail := payload.View.Detail
			if detail["kind"] != "api_rejected" || detail["intakeHttpStatus"] != "409" || !strings.Contains(logs.String(), `intake_http_status="409"`) {
				t.Fatalf("detail = %#v, log = %s", detail, logs.String())
			}
			if code == "request_failed" {
				if detail["intakeCode"] != code || !strings.Contains(logs.String(), `intake_code="request_failed"`) {
					t.Fatalf("detail = %#v, log = %s", detail, logs.String())
				}
			} else if _, ok := detail["intakeCode"]; ok || strings.Contains(logs.String(), code) || strings.Contains(string(body), code) {
				t.Fatal("unconfirmed code disclosed")
			}
			for _, secret := range []string{"secret-wrapper", "https://example.invalid", "Bearer", "token"} {
				if strings.Contains(string(body), secret) || strings.Contains(logs.String(), secret) {
					t.Fatalf("unsafe text %q disclosed", secret)
				}
			}
		})
	}
}

func TestGiteaStatusReportAndLogPrivacy(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	var body []byte
	calls := 0
	api := leaseClient(t, func(req *http.Request) (*http.Response, error) {
		calls++
		body, _ = io.ReadAll(req.Body)
		return leaseResponse(204, "")
	})
	cause := &gitea.Error{Status: 503, Message: "secret-message https://example.invalid Bearer token"}
	wrapped := &execution.ExecutionError{Err: fmt.Errorf("secret-wrapper: %w", cause), Stage: "publish"}
	runner := Runner{API: api, Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
		return intake.State{}, wrapped
	}}, RenewInterval: time.Hour, Now: func() time.Time { return now }}
	var logs bytes.Buffer
	old := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(old)
	if err := runner.execute(context.Background(), assignment(now)); !errors.Is(err, cause) {
		t.Fatalf("error = %v", err)
	}
	var payload struct {
		View struct {
			Detail map[string]string `json:"lastExecutionError"`
		} `json:"view"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || len(payload.View.Detail) != 4 || payload.View.Detail["at"] != "2026-11-01T00:00:00Z" || payload.View.Detail["giteaHttpStatus"] != "503" || payload.View.Detail["kind"] != "gitea_api_error" || payload.View.Detail["stage"] != "publish" || payload.View.Detail["reason"] != "" {
		t.Fatalf("calls = %d, detail = %#v", calls, payload.View.Detail)
	}
	if !strings.Contains(logs.String(), "gitea_http_status=503") {
		t.Fatal("status absent from log")
	}
	for _, secret := range []string{"secret-message", "https://example.invalid", "Bearer", "token", "secret-wrapper"} {
		if strings.Contains(string(body), secret) || strings.Contains(logs.String(), secret) {
			t.Fatalf("unsafe text %q disclosed", secret)
		}
	}
}

func TestExecutionDiagnosticReportAndLogPrivacy(t *testing.T) {
	const allowed = "公開PRのリポジトリ・枝・既定ブランチ・headが一致しません"
	for _, tc := range []struct{ message, stage, reason string }{
		{allowed, "publish", allowed}, {allowed, "", allowed},
		{"API response secret", "publish", ""}, {"https://example.invalid/?token=secret", "publish", ""},
		{"Bearer secret", "", ""}, {allowed + " secret", "publish", ""},
	} {
		t.Run(tc.message+tc.stage, func(t *testing.T) {
			now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
			var body []byte
			api := leaseClient(t, func(req *http.Request) (*http.Response, error) {
				body, _ = io.ReadAll(req.Body)
				return leaseResponse(204, "")
			})
			cause := errors.New(tc.message)
			wrapped := &execution.ExecutionError{Err: cause, Stage: tc.stage}
			runner := Runner{API: api, Executor: fakeExecutor{fn: func(context.Context, func(context.Context) error) (intake.State, error) {
				return intake.State{}, wrapped
			}}, RenewInterval: time.Hour, Now: func() time.Time { return now }}
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			err := runner.execute(context.Background(), assignment(now))
			if !errors.Is(err, cause) || jobErrorKind(err) != jobErrorKind(cause) {
				t.Fatal("error classification changed")
			}
			var payload struct {
				View struct {
					Detail map[string]string `json:"lastExecutionError"`
				} `json:"view"`
			}
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			detail := payload.View.Detail
			if detail["stage"] != tc.stage || detail["reason"] != tc.reason || detail["kind"] != "execution_error" {
				t.Fatalf("detail: %#v", detail)
			}
			if tc.stage == "" {
				if _, ok := detail["stage"]; ok {
					t.Fatal("empty stage present")
				}
			} else if !strings.Contains(logs.String(), `stage="`+tc.stage+`"`) {
				t.Fatal("stage absent from log")
			}
			if tc.reason == "" {
				if _, ok := detail["reason"]; ok || strings.Contains(string(body), tc.message) || strings.Contains(logs.String(), tc.message) {
					t.Fatal("unsafe reason disclosed")
				}
			} else if !strings.Contains(logs.String(), tc.reason) {
				t.Fatal("reason absent from log")
			}
		})
	}
}

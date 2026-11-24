package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/process"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var metricKeyPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

type LiveCheck struct {
	Repo           string   `json:"repo"`
	Path           string   `json:"path"`
	Command        []string `json:"command"`
	TimeoutSeconds int      `json:"timeoutSeconds"`
	checkout       bool
}
type liveResult struct {
	Pass     bool               `json:"pass"`
	Summary  string             `json:"summary"`
	Metrics  map[string]float64 `json:"metrics"`
	Failures []liveFailureItem  `json:"failures"`
}
type liveFailureItem struct {
	Target string `json:"target"`
	Reason string `json:"reason"`
}
type livePending struct {
	ID     int64      `json:"id"`
	Result liveResult `json:"result"`
}

func liveFailure(reason string) liveResult {
	return liveResult{false, "実地確認を実行できませんでした", map[string]float64{}, []liveFailureItem{{"live_check", reason}}}
}

var unsafeLiveText = regexp.MustCompile(`(?i)[\x00-\x1f\x7f-\x{009f}]|data:|base64|iVBORw0KGgo|/9j/|R0lGOD|<img|\b(?:bytes|thumbnail|password|authorization|credential|secret|token)\b|\b(?:idk_|idh_|sk-)|https?:|[?&][^\s=]+\s*=|[A-Za-z0-9+/=_-]{32,}|\\x[0-9a-f]{2}|\x{fffd}`)

func safeLiveText(s string) bool { return !unsafeLiveText.MatchString(s) }
func validateLive(raw []byte) (liveResult, error) {
	var result liveResult
	if len(raw) > 16384 || !utf8.Valid(raw) {
		return result, errors.New("invalid_result")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 4 || (string(fields["pass"]) != "true" && string(fields["pass"]) != "false") || fields["summary"] == nil || fields["metrics"] == nil || fields["failures"] == nil {
		return result, errors.New("invalid_result")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.Metrics == nil || result.Failures == nil || len(result.Metrics) > 100 || len(result.Failures) > 20 || utf8.RuneCountInString(result.Summary) < 1 || utf8.RuneCountInString(result.Summary) > 500 {
		return result, errors.New("invalid_result")
	}
	if result.Pass && len(result.Failures) != 0 || !result.Pass && len(result.Failures) == 0 || !safeLiveText(result.Summary) {
		return result, errors.New("unsafe_result")
	}
	for key, n := range result.Metrics {
		if !metricKeyPattern.MatchString(key) || math.IsNaN(n) || math.IsInf(n, 0) {
			return result, errors.New("invalid_result")
		}
	}
	for _, f := range result.Failures {
		if utf8.RuneCountInString(f.Target) < 1 || utf8.RuneCountInString(f.Target) > 200 || utf8.RuneCountInString(f.Reason) < 1 || utf8.RuneCountInString(f.Reason) > 500 || !safeLiveText(f.Target) || !safeLiveText(f.Reason) {
			return result, errors.New("unsafe_result")
		}
	}
	return result, nil
}
func (r *Runtime) runLiveChecks(ctx context.Context) error {
	name := r.liveStateName
	if name == "" {
		name = "live-state.json"
	}
	var pending *livePending
	if err := r.load(name, &pending); err != nil {
		return err
	}
	if pending != nil {
		if pending.ID < 1 {
			return errors.New("実地確認の保存IDが不正です")
		}
		if _, err := validateLive(encode(pending.Result)); err != nil {
			return err
		}
		if err := r.api(ctx, "POST", "live-check-result", pending, nil); err != nil {
			return err
		}
		if err := r.save(name, nil); err != nil {
			return err
		}
	}
	if len(r.Config.LiveChecks) == 0 {
		return nil
	}
	var next struct {
		Job *struct {
			ID      int64           `json:"id"`
			Repo    string          `json:"repo"`
			Commit  string          `json:"commit"`
			Host    string          `json:"host"`
			Command []string        `json:"command"`
			Timeout int             `json:"timeoutSeconds"`
			Failure json.RawMessage `json:"failure"`
		} `json:"job"`
	}
	if err := r.api(ctx, "GET", "live-check-next", nil, &next); err != nil {
		return err
	}
	if next.Job == nil {
		return nil
	}
	job := next.Job
	if job.ID < 1 || !shaPattern.MatchString(job.Commit) {
		return errors.New("実地確認の依頼が不正です")
	}
	pending = &livePending{job.ID, liveFailure("interrupted")}
	if err := r.save(name, pending); err != nil {
		return err
	}
	run := func() liveResult {
		var local *LiveCheck
		for i := range r.Config.LiveChecks {
			if r.Config.LiveChecks[i].Repo == job.Repo {
				if local != nil {
					return liveFailure("not_allowed")
				}
				local = &r.Config.LiveChecks[i]
			}
		}
		if local == nil || !filepath.IsAbs(local.Path) || !reflect.DeepEqual(local.Command, job.Command) || job.Timeout != local.TimeoutSeconds || job.Host != r.Config.Host || job.Timeout < 1 || job.Timeout > 3600 || !argv(job.Command) || len(job.Command) > 64 {
			return liveFailure("not_allowed")
		}
		for _, arg := range job.Command {
			if arg == "" || len(arg) > 1000 || strings.ContainsAny(arg, "\r\n\x00") || !safeLiveText(arg) {
				return liveFailure("not_allowed")
			}
		}
		if len(job.Failure) > 0 && string(job.Failure) != "null" && string(job.Failure) != "false" {
			return liveFailure("deployment_failed")
		}
		if local.checkout {
			if err := r.checkoutLive(ctx, local.Path, job.Commit); err != nil {
				fmt.Fprintf(os.Stderr, "実地確認の取り込みに失敗しました: %v\n", err)
				return liveFailure("checkout_failed")
			}
		}
		head, err := r.git(ctx, local.Path, "rev-parse", "HEAD")
		if err != nil || head != job.Commit {
			return liveFailure("revision_mismatch")
		}
		out, err := r.Run(ctx, local.Command[0], local.Command[1:], process.Options{Dir: local.Path, Timeout: time.Duration(job.Timeout) * time.Second, MaxOutputBytes: 16384, Env: map[string]string{"ISSUE_DESK_LIVE_COMMIT": job.Commit}})
		if errors.Is(err, process.ErrOutputLimit) {
			return liveFailure("output_limit")
		}
		if err != nil {
			if out.Interrupted {
				return liveFailure("timeout")
			}
			return liveFailure("start_failed")
		}
		if out.Code != 0 {
			return liveFailure("exit_failed")
		}
		result, err := validateLive([]byte(out.Out))
		if err != nil {
			return liveFailure(err.Error())
		}
		return result
	}
	pending.Result = run()
	if err := r.save(name, pending); err != nil {
		return err
	}
	if err := r.api(ctx, "POST", "live-check-result", pending, nil); err != nil {
		return err
	}
	return r.save(name, nil)
}
func encode(v any) json.RawMessage { data, _ := json.Marshal(v); return data }

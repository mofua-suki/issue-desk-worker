package hostd

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/atomicfile"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/process"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var envLinePattern = regexp.MustCompile(`^\s*(?:export\s+)?([A-Z_]+)\s*=\s*(.*?)\s*$`)

type cycleFailure struct{ Stage, Reason, Detail string }

var ErrLocked = errors.New("hostdの周期処理が動いています")

func (e *cycleFailure) Error() string { return "hostdの処理が失敗しました" }

type Runtime struct {
	liveStateName string
	failure       *cycleFailure
	Config        Configuration
	HTTP          *http.Client
	token         string
	lock          *os.File
	Run           func(context.Context, string, []string, process.Options) (process.Result, error)
	Now           func() time.Time
	Sleep         func(context.Context, time.Duration) error
}

func New(c Configuration) (*Runtime, error) {
	token, err := secret(filepath.Join(c.Home, "token"), hostTokenPattern)
	if err != nil {
		return nil, err
	}
	if _, err := secret(filepath.Join(c.Home, "registry-password"), nil); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(c.ClientDir, ".linux-hostd.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, cause.New("hostdの実行ロックを開けません", err)
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		return nil, ErrLocked
	}
	return &Runtime{Config: c, token: token, lock: f, HTTP: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, Run: process.Run, Now: time.Now, Sleep: func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}}, nil
}
func (r *Runtime) Close() error {
	if r.lock == nil {
		return nil
	}
	return r.lock.Close()
}
func (r *Runtime) api(ctx context.Context, method, route string, payload, out any) error {
	var raw []byte
	if payload != nil {
		var err error
		raw, err = json.Marshal(payload)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, r.Config.URL+"/api/v1/hosts/"+r.Config.Host+"/"+route, bytes.NewReader(raw))
	if err != nil {
		return cause.New("hostd要求が不正です", err)
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := r.HTTP.Do(req)
	if err != nil {
		return cause.New("hostd APIへ接続できません", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &apiStatusError{response.StatusCode}
	}
	if out != nil {
		data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20+1))
		if err != nil || len(data) > 2<<20 || json.Unmarshal(data, out) != nil {
			return cause.New("hostd応答が不正です", err)
		}
	}
	return nil
}
func (r *Runtime) load(name string, out any) error {
	data, err := os.ReadFile(filepath.Join(r.Config.Home, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || json.Unmarshal(data, out) != nil {
		return cause.New("hostdの保存状態を読めません", err)
	}
	return nil
}
func (r *Runtime) save(name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(r.Config.Home, name), ".state-", 0600, true, raw, nil)
}
func (r *Runtime) command(ctx context.Context, cwd, name string, args []string, input string, timeout time.Duration) (string, error) {
	out, err := r.Run(ctx, name, args, process.Options{Dir: cwd, Input: input, Timeout: timeout, Env: map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"}})
	if err != nil || out.Code != 0 {
		reason := "other"
		stage := "client"
		if name == "docker" || name == "bash" {
			reason = "docker_exit"
			stage = "docker"
		}
		if out.Interrupted {
			reason = "timeout"
		}
		fmt.Fprintln(os.Stderr, commandFailureLine(name, args, input, out, err))
		return "", &cycleFailure{stage, reason, "cycle_failed"}
	}
	return strings.TrimSpace(out.Out), nil
}

// commandStderrLimit keeps the end of a failed command's stderr, where docker
// and bash put the reason, without flooding the hostd log.
const commandStderrLimit = 2000

// commandFailureLine names the failed command and keeps the end of its stderr.
// The cycle report only carries fixed codes, so this log line is where the
// cause survives. Standard input (the registry password) and credentials in
// URLs are hidden.
func commandFailureLine(name string, args []string, input string, out process.Result, err error) string {
	target := name
	if len(args) > 0 {
		target += " " + filepath.Base(args[0])
	}
	detail := strings.TrimSpace(out.Err)
	if detail == "" && err != nil {
		detail = err.Error()
	}
	// Hide before cutting, so a credential at the cut is not left half shown.
	if secret := strings.TrimSpace(input); secret != "" {
		detail = strings.ReplaceAll(detail, secret, "[REDACTED]")
	}
	detail = process.HideURLCredentials(detail)
	if len(detail) > commandStderrLimit {
		detail = "…" + strings.ToValidUTF8(detail[len(detail)-commandStderrLimit:], "")
	}
	if detail == "" {
		detail = "標準エラーなし"
	}
	state := fmt.Sprintf("終了コード%d", out.Code)
	if out.Interrupted {
		state = "時間切れ"
	}
	return fmt.Sprintf("hostdの%sが失敗しました（%s）: %s", target, state, detail)
}

type environment struct{ Flavor, Tag string }

func (r *Runtime) environment() (environment, error) {
	raw, err := os.ReadFile(filepath.Join(r.Config.ClientDir, ".env"))
	if err != nil {
		return environment{}, cause.New("Workerの環境ファイルを読めません", err)
	}
	values := map[string]string{}
	linePattern := envLinePattern
	for _, line := range strings.Split(string(raw), "\n") {
		m := linePattern.FindStringSubmatch(strings.TrimSuffix(line, "\r"))
		if len(m) == 0 {
			continue
		}
		if _, ok := values[m[1]]; ok {
			return environment{}, errors.New("環境変数が重複しています")
		}
		v := m[2]
		if len(v) >= 2 && (v[0] == '\'' && v[len(v)-1] == '\'' || v[0] == '"' && v[len(v)-1] == '"') {
			v = v[1 : len(v)-1]
		}
		values[m[1]] = v
	}
	e := environment{values["FLAVOR"], values["ISSUE_DESK_CLIENT_TAG"]}
	if e.Flavor != "" && e.Flavor != "-python" || !tagPattern.MatchString(e.Tag) {
		return e, errors.New("Workerの環境が不正です")
	}
	return e, nil
}
func (r *Runtime) update(ctx context.Context, tag string, extra ...string) bool {
	input := ""
	if tag != "--restore-previous" && tag != "--mark-green-stopped" {
		value, err := secret(filepath.Join(r.Config.Home, "registry-password"), nil)
		if err != nil {
			// The update is not attempted; say why instead of only failing.
			fmt.Fprintf(os.Stderr, "hostdの更新を始められません: %v\n", err)
			if r.failure == nil {
				r.failure = &cycleFailure{"client", "other", "cycle_failed"}
			}
			return false
		}
		input = value + "\n"
	}
	_, err := r.command(ctx, r.Config.ClientDir, "bash", append([]string{filepath.Join(r.Config.ClientDir, "update-linux.sh"), r.Config.ClientDir, tag}, extra...), input, 15*time.Minute)
	if err != nil && r.failure == nil {
		reason := "other"
		var failure *cycleFailure
		if errors.As(err, &failure) {
			reason = failure.Reason
		}
		stage, detail := "swap-start", "start_failed"
		if tag == "--restore-previous" {
			stage = "recover"
			detail = "restore_failed"
		}
		r.failure = &cycleFailure{stage, reason, detail}
	}
	return err == nil
}

// composeImage and imageDigests share command execution and JSON parsing while
// callers retain their own error handling.
func (r *Runtime) composeImage(ctx context.Context, envFile, composeFile string) (string, error) {
	args := []string{"compose", "--project-directory", r.Config.ClientDir, "--env-file", envFile, "-f", composeFile, "config", "--format", "json"}
	raw, err := r.command(ctx, r.Config.ClientDir, "docker", args, "", time.Minute)
	if err != nil {
		return "", err
	}
	var compose struct {
		Services map[string]struct {
			Image string `json:"image"`
		} `json:"services"`
	}
	if json.Unmarshal([]byte(raw), &compose) != nil {
		return "", errors.New("復旧用imageを確認できません")
	}
	return compose.Services["issue-desk-client"].Image, nil
}
func (r *Runtime) imageDigests(ctx context.Context, image string) ([]string, error) {
	raw, err := r.command(ctx, r.Config.ClientDir, "docker", []string{"image", "inspect", "--format", "{{json .RepoDigests}}", image}, "", time.Minute)
	if err != nil {
		return nil, err
	}
	var digests []string
	if json.Unmarshal([]byte(raw), &digests) != nil {
		return nil, errors.New("復旧用digestを読めません")
	}
	return digests, nil
}
func (r *Runtime) verifyDigest(ctx context.Context, t Target) bool {
	actual, err := r.composeImage(ctx, filepath.Join(r.Config.ClientDir, ".env"), filepath.Join(r.Config.ClientDir, "compose.yaml"))
	image := t.Registry + "/issue-desk-client" + t.Flavor + ":" + t.Tag
	if err != nil || actual != image {
		return false
	}
	digests, err := r.imageDigests(ctx, image)
	if err != nil {
		return false
	}
	for _, digest := range digests {
		if digest == t.Registry+"/issue-desk-client"+t.Flavor+"@"+t.Digest {
			return true
		}
	}
	return false
}

func (r *Runtime) Cycle(ctx context.Context) (err error) {
	r.failure = nil
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		return err
	}
	result := "success"
	reason := ""
	defer func() {
		if err != nil || r.failure != nil {
			result = "failed"
			reason = "other"
			if r.failure == nil {
				var failure *cycleFailure
				if errors.As(err, &failure) {
					r.failure = failure
				} else {
					r.failure = &cycleFailure{"client", "other", "cycle_failed"}
				}
			}
		}
		env, readErr := r.environment()
		if readErr != nil {
			return
		}
		cycle := map[string]any{"id": hex.EncodeToString(id), "phase": "finish", "result": result, "stage": "complete", "exceptionType": nil, "at": r.Now().UTC().Format(time.RFC3339Nano), "versions": map[string]any{"current": env.Tag, "previous": nil, "lastGood": nil}}
		if reason != "" {
			cycle["reason"] = r.failure.Reason
			cycle["stage"] = r.failure.Stage
			cycle["detail"] = r.failure.Detail
		}
		reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		reportErr := r.api(reportCtx, "POST", "hostd-health", map[string]any{"cycle": cycle}, nil)
		if err == nil {
			err = reportErr
		}
	}()
	// A failed update does not skip the other stages: they only run when the
	// saved update state says no swap is in progress, read below.
	updateErr := r.updateCycle(ctx)
	if ctx.Err() != nil {
		return errors.Join(updateErr, ctx.Err())
	}
	var state *UpdateState
	if err = r.load("state.json", &state); err != nil {
		return errors.Join(updateErr, err)
	}
	if state != nil && !(state.Phase == "waiting" && !state.DrainSet) {
		return updateErr
	}
	// Apps, readers and live checks are independent: one failing does not
	// skip the others. The first failure's code is the one reported.
	appsErr := r.runApps(ctx)
	if ctx.Err() != nil {
		return errors.Join(updateErr, appsErr, ctx.Err())
	}
	readersErr := r.runReaders(ctx)
	if ctx.Err() != nil {
		return errors.Join(updateErr, appsErr, readersErr, ctx.Err())
	}
	return errors.Join(updateErr, appsErr, readersErr, r.runLiveChecks(ctx))
}

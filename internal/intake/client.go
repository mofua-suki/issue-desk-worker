// Package intake contains only HTTP contracts. It never imports server implementation code.
package intake

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/apipath"
	"issue-desk-worker/internal/cause"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"issue-desk-worker/internal/config"
)

var ErrJobReleased = errors.New("受付が仕事を保存してleaseを解放しました")

const ContractHeader = "x-issue-desk-contract"
const ContractVersion = "4cd4625bbadfe9da0aa2a3f0af5ead6a3c8b12da3ef27b3d1f751c2ca9759213"
const maxResponseBytes = 8 << 20

type Client struct {
	base  string
	token string
	host  string
	http  *http.Client
}

type APIError struct {
	Status int
	Code   string
	// Call is the method and path template of the refused request.
	Call string
}

func (e *APIError) Error() string {
	if e.Call == "" {
		return fmt.Sprintf("受付APIがHTTP %dを返しました（%s）", e.Status, e.Code)
	}
	return fmt.Sprintf("受付APIがHTTP %dを返しました（%s、%s）", e.Status, e.Code, e.Call)
}

// CauseKind names the refusal; it carries only the status, code and call.
func (e *APIError) CauseKind() string { return e.Error() }

// LeaseLost reports whether the API rejected a lost lease. Changes require
// reviewing runtime's branch-order tests.
func (e *APIError) LeaseLost() bool { return e.Status == 409 && e.Code == "lease_lost" }

// TransportError retains only a safe classification and the method and path
// template of the request, never the URL, its values or the underlying error.
type TransportError struct {
	Timeout bool
	// Call is the method and path template of the request, or empty.
	Call string
}

// CauseKind names the failure; it carries only the classification, not the
// request, so the kind stays one fixed value per classification.
func (e *TransportError) CauseKind() string {
	if e.Timeout {
		return "受付APIの要求が時間切れになりました"
	}
	return "受付APIへの接続に失敗しました"
}

func (e *TransportError) Error() string {
	if e.Call == "" {
		return e.CauseKind()
	}
	return e.CauseKind() + "（" + e.Call + "）"
}

// TemporaryFailureReason classifies temporary lease and acquisition failures.
// Call and acquisition never retry a request.
// Changes require reviewing runtime's branch-order tests.
func TemporaryFailureReason(err error) string {
	var transport *TransportError
	if errors.As(err, &transport) {
		if transport.Timeout {
			return "request_timeout"
		}
		return "connection_failed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "request_timeout"
	}
	var api *APIError
	if errors.As(err, &api) && api.Status >= 500 && api.Status <= 599 && api.Code != "contract_mismatch" && api.Code != "CONTRACT_MISMATCH" {
		return "http_5xx"
	}
	return ""
}

// callName is the method and path template of a request, safe for messages.
func callName(method, path string) string { return method + " " + apipath.Template(path) }

func transportFailure(ctx context.Context, err error, call string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var certificate *tls.CertificateVerificationError
	if errors.As(err, &certificate) {
		return errors.New("受付APIの証明書を検証できません")
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return &TransportError{Timeout: true, Call: call}
	}
	// Any other failure to get a response (a proxy's malformed reply, a
	// reset in the HTTP layer) is a connection failure that a later attempt
	// may not meet. Only certificate failures above stay permanent.
	return &TransportError{Call: call}
}

func New(b config.Bootstrap, token string, transport http.RoundTripper) (*Client, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	if !config.ValidToken(token) {
		return nil, errors.New("PC別トークンが不正です")
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &Client{base: strings.TrimRight(b.ServerURL, "/"), token: token, host: b.WorkerID, http: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// call never retries a request. In particular, a lost /jobs/next response must not take a second job.
// Server text and network errors may contain secrets and are deliberately not copied into errors.
func (c *Client) call(ctx context.Context, method, path string, payload any, out any) error {
	return c.send(ctx, method, path, payload, out, path == "/api/v1/jobs/next", 0)
}

// Call sends one JSON request to an API path of the configured Server and
// decodes a 2xx body into out. Like call it never retries; it also sends the
// contract version so a mismatched Server refuses with CONTRACT_MISMATCH.
// A refusal is an *APIError whose Code is one of the fixed values call keeps
// (unknown codes become request_failed); a 204 or empty out decodes nothing.
func (c *Client) Call(ctx context.Context, method, path string, payload any, out any) error {
	if err := ValidateTarget(path); err != nil {
		return err
	}
	return c.send(ctx, method, path, payload, out, true, MaxRequestBytes)
}

// send is the single request path; limit > 0 bounds the encoded body.
func (c *Client) send(ctx context.Context, method, path string, payload any, out any, contract bool, limit int) error {
	call := callName(method, path)
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return cause.New("送信JSONを作れません", err)
		}
		if limit > 0 && len(data) > limit {
			return fmt.Errorf("要求本文が上限を超えました（%s）", call)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return cause.New("受付APIの要求を作れません", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if contract {
		req.Header.Set(ContractHeader, ContractVersion)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return transportFailure(ctx, err, call)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		return fmt.Errorf("受付APIの転送を拒否しました（%s）", call)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		// A body read failure must not turn an observed authorization/contract
		// refusal into a retryable connection failure.
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return &APIError{Status: res.StatusCode, Code: "request_failed", Call: call}
		}
		return transportFailure(ctx, err, call)
	}
	if len(data) > maxResponseBytes {
		return fmt.Errorf("受付APIの応答が上限を超えました（%s）", call)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		var p struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &p)
		switch p.Code {
		case "lease_lost", "turn_lost", "contract_mismatch", "CONTRACT_MISMATCH":
		default:
			p.Code = "request_failed"
		}
		return &APIError{Status: res.StatusCode, Code: p.Code, Call: call}
	}
	if res.StatusCode == http.StatusNoContent {
		return nil
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return cause.New("受付APIのJSON応答が不正です（"+call+"）", err)
	}
	return nil
}

type VerifierState struct {
	Known     bool `json:"known"`
	Available bool `json:"available"`
}
type ConfigSnapshot struct {
	WebhookVerifierState *VerifierState  `json:"webhookVerifierState,omitempty"`
	Version              string          `json:"version"`
	ConfigurationVersion string          `json:"configurationVersion"`
	SettingsVersion      string          `json:"settingsVersion"`
	Config               json.RawMessage `json:"config"`
	Settings             json.RawMessage `json:"settings"`
	ReviewNotes          json.RawMessage `json:"reviewNotes"`
}

func (c *Client) Config(ctx context.Context) (ConfigSnapshot, error) {
	var out ConfigSnapshot
	err := c.call(ctx, "GET", ConfigPath(c.host), nil, &out)
	if err != nil {
		return out, err
	}
	if err := out.Validate(); err != nil {
		return ConfigSnapshot{}, err
	}
	return out, nil
}

// ConfigPath is the request path of host's configuration snapshot.
func ConfigPath(host string) string { return "/api/v1/client-config?host=" + url.QueryEscape(host) }

// Validate reports a snapshot that lacks a version or the configuration.
func (s ConfigSnapshot) Validate() error {
	if s.Version == "" || s.ConfigurationVersion == "" || s.SettingsVersion == "" || len(s.Config) == 0 || string(s.Config) == "null" {
		return errors.New("client-configの必要な項目がありません")
	}
	return nil
}

type HostStatus struct {
	CLIVersions   []map[string]any   `json:"cliVersions"`
	IntakeHold    map[string]any     `json:"intakeHold"`
	Revision      *string            `json:"revision"`
	ImageRevision *string            `json:"imageRevision"`
	Load          map[string]float64 `json:"load"`
	Busy          bool               `json:"busy"`
	Implementing  int                `json:"implementing"`
	Publishing    int                `json:"publishing"`
	Capabilities  map[string]any     `json:"capabilities"`
	Ready         bool               `json:"ready"`
	Checks        map[string]any     `json:"checks"`
	Capacity      int                `json:"capacity"`
}

func (c *Client) Heartbeat(ctx context.Context, status HostStatus) error {
	if status.Capacity < 1 || status.Capacity > 8 || status.Implementing < 0 || status.Publishing < 0 {
		return errors.New("heartbeatの枠または件数が不正です")
	}
	return c.call(ctx, "PUT", "/api/v1/hosts/"+url.PathEscape(c.host), status, nil)
}

type Lease struct {
	Epoch     int64     `json:"epoch"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (l Lease) Valid(now time.Time) bool { return l.Epoch > 0 && l.ExpiresAt.After(now) }

type Job struct {
	LeaseEpoch int64           `json:"leaseEpoch"`
	ID         int64           `json:"id"`
	Repo       string          `json:"repo"`
	Status     string          `json:"status"`
	Raw        json.RawMessage `json:"-"`
}

func (j *Job) UnmarshalJSON(data []byte) error {
	type plain Job
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*j = Job(p)
	j.Raw = append(j.Raw[:0], data...)
	return nil
}

type Assignment struct {
	Job   *Job   `json:"job"`
	Lease *Lease `json:"lease"`
}

func (a Assignment) Validate(now time.Time) error {
	if a.Job == nil {
		if a.Lease != nil {
			return errors.New("仕事のない応答にleaseがあります")
		}
		return nil
	}
	if a.Job.ID < 1 || a.Job.Repo == "" || a.Lease == nil || !a.Lease.Valid(now) {
		return errors.New("仕事のIDまたはleaseが不正です")
	}
	return nil
}
func (c *Client) Next(ctx context.Context, capacity int) (Assignment, error) {
	return c.NextFor(ctx, capacity, nil)
}
func (c *Client) NextFor(ctx context.Context, capacity int, repos []string) (Assignment, error) {
	if capacity < 1 || capacity > 8 {
		return Assignment{}, errors.New("capacityは1から8にしてください")
	}
	var out Assignment
	payload := map[string]any{"capacity": capacity}
	if repos != nil {
		payload["repos"] = repos
	}
	err := c.call(ctx, "POST", "/api/v1/jobs/next", payload, &out)
	return out, err
}
func jobPath(id int64, suffix string) (string, error) {
	if id < 1 {
		return "", errors.New("ジョブIDが不正です")
	}
	return "/api/v1/jobs/" + strconv.FormatInt(id, 10) + suffix, nil
}
func (c *Client) Renew(ctx context.Context, id, epoch int64) (Assignment, error) {
	p, err := jobPath(id, "/lease")
	if err != nil {
		return Assignment{}, err
	}
	if epoch < 1 {
		return Assignment{}, errors.New("epochが不正です")
	}
	var out Assignment
	err = c.call(ctx, "POST", p, map[string]int64{"epoch": epoch}, &out)
	return out, err
}

type State struct {
	ControllerTest   json.RawMessage `json:"controllerTest,omitempty"`
	ControllerReport json.RawMessage `json:"controllerReport,omitempty"`
	Status           string          `json:"status"`
	HoldReason       string          `json:"holdReason,omitempty"`
	View             json.RawMessage `json:"view,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	Branch           string          `json:"branch,omitempty"`
	HeadSHA          string          `json:"headSha,omitempty"`
	// DiffFingerprint is sent only with HeadSHA; empty omits it.
	DiffFingerprint string `json:"diffFingerprint,omitempty"`
	PR              *int64 `json:"pr,omitempty"`
}

func (c *Client) State(ctx context.Context, id, epoch int64, state State) error {
	p, err := jobPath(id, "/state")
	if err != nil {
		return err
	}
	if epoch < 1 {
		return errors.New("epochが不正です")
	}
	switch state.Status {
	case "running", "held", "failed", "merged", "stopped", "queued":
	default:
		return errors.New("送信できないジョブ状態です")
	}
	body := struct {
		Epoch int64 `json:"epoch"`
		State
	}{epoch, state}
	return c.call(ctx, "POST", p, body, nil)
}

// Note reports one review observation under the current lease.
func (c *Client) Note(ctx context.Context, id, epoch int64, kind string, data map[string]any) error {
	p, err := jobPath(id, "/notes")
	if err != nil {
		return err
	}
	if epoch < 1 {
		return errors.New("epochが不正です")
	}
	if kind != "finding" && kind != "followup" && kind != "improvement" {
		return errors.New("注意点の種類が不正です")
	}
	if data == nil {
		return errors.New("注意点のdataが必要です")
	}
	return c.call(ctx, "POST", p, map[string]any{"epoch": epoch, "kind": kind, "data": data}, nil)
}

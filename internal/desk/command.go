// Package desk implements explicit token-authenticated API operations without screen-session fallback.
package desk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/redact"
	"os"
	"regexp"
	"strings"

	"issue-desk-worker/internal/config"
	"issue-desk-worker/internal/intake"
)

// Factory exists so callers can configure the HTTPS transport without changing authentication.
type Factory func(config.Bootstrap, string) (*intake.Client, error)

type Command struct {
	In        io.Reader
	Out       io.Writer
	Err       io.Writer
	NewClient Factory
}

type arguments struct{ mode, config, target, method, bodyFile string }

const usage = "使い方: desk doctor --config <設定> | desk get --config <設定> --path </api/...> | desk operation --config <設定> --method <POST|PUT|PATCH|DELETE> --path </api/...> [--body-file <JSONファイル|->]"

func parse(args []string) (arguments, error) {
	var out arguments
	if len(args) == 0 {
		return out, errors.New(usage)
	}
	out.mode = args[0]
	allowed := map[string]bool{"--config": true}
	switch out.mode {
	case "doctor":
	case "get":
		allowed["--path"] = true
		out.method = "GET"
	case "operation":
		allowed["--path"] = true
		allowed["--method"] = true
		allowed["--body-file"] = true
	default:
		return out, errors.New(usage)
	}
	values := map[string]string{}
	for i := 1; i < len(args); i += 2 {
		key := args[i]
		if !allowed[key] || i+1 >= len(args) || values[key] != "" || args[i+1] == "" || strings.HasPrefix(args[i+1], "--") {
			return out, errors.New("引数の種類・重複・値を確認してください")
		}
		values[key] = args[i+1]
	}
	out.config = values["--config"]
	out.target = values["--path"]
	out.bodyFile = values["--body-file"]
	if out.config == "" {
		return out, errors.New("--configで3項目の設定ファイルを指定してください")
	}
	if out.mode != "doctor" {
		if err := intake.ValidateTarget(out.target); err != nil {
			return out, err
		}
	}
	if out.mode == "operation" {
		out.method = values["--method"]
		switch out.method {
		case "POST", "PUT", "PATCH", "DELETE":
		default:
			return out, errors.New("operationにはPOST・PUT・PATCH・DELETEのいずれかを指定してください")
		}
	}
	return out, nil
}

// Run returns 0 for successful HTTP responses, 1 for HTTP refusal, and 2 for local/transport errors.
// Server errors are written as a structured response, not collapsed into a connection error.
func (c Command) Run(ctx context.Context, args []string) int {
	if c.In == nil {
		c.In = strings.NewReader("")
	}
	if c.Out == nil {
		c.Out = io.Discard
	}
	if c.Err == nil {
		c.Err = io.Discard
	}
	fail := func(err error) int { fmt.Fprintln(c.Err, err); return 2 }
	a, err := parse(args)
	if err != nil {
		return fail(err)
	}
	var body json.RawMessage
	if a.bodyFile != "" {
		input := c.In
		if a.bodyFile != "-" {
			f, err := os.Open(a.bodyFile)
			if err != nil {
				return fail(cause.New("要求本文ファイルを開けません", err))
			}
			defer f.Close()
			input = f
		}
		data, err := io.ReadAll(io.LimitReader(input, intake.MaxRequestBytes+1))
		if err != nil || len(data) > intake.MaxRequestBytes {
			return fail(cause.New("要求本文を上限内で読めません", err))
		}
		if !json.Valid(data) {
			return fail(errors.New("要求本文のJSONが不正です"))
		}
		body = data
	}
	b, err := config.Load(a.config)
	if err != nil {
		return fail(err)
	}
	token, err := b.ReadToken()
	if err != nil {
		return fail(err)
	}
	factory := c.NewClient
	if factory == nil {
		factory = func(b config.Bootstrap, token string) (*intake.Client, error) { return intake.New(b, token, nil) }
	}
	api, err := factory(b, token)
	if err != nil {
		return fail(err)
	}
	if a.mode == "doctor" {
		// Read the configuration as an explicit request, so an HTTP refusal or
		// redirect exits 1 with its status like get, apart from a transport
		// failure (2).
		response, err := api.Explicit(ctx, "GET", intake.ConfigPath(b.WorkerID), nil)
		if err != nil {
			if response.Status != 0 {
				_ = json.NewEncoder(c.Out).Encode(map[string]any{"status": response.Status, "body": nil, "bodyType": "unavailable"})
			}
			return fail(err)
		}
		if response.Status < 200 || response.Status >= 300 {
			if err := writeResponse(c.Out, response, token); err != nil {
				return fail(cause.New("API応答を出力できません", err))
			}
			return 1
		}
		var snapshot intake.ConfigSnapshot
		if err := json.Unmarshal(response.Body, &snapshot); err != nil {
			return fail(cause.New("受付の設定の応答が不正です", err))
		}
		if err := snapshot.Validate(); err != nil {
			return fail(err)
		}
		summary, redacted := scrub(map[string]any{"workerId": b.WorkerID, "configVersion": snapshot.Version, "configurationVersion": snapshot.ConfigurationVersion}, token)
		if redacted {
			summary.(map[string]any)["redacted"] = true
		}
		if err := json.NewEncoder(c.Out).Encode(summary); err != nil {
			return fail(cause.New("診断結果を出力できません", err))
		}
		return 0
	}
	response, err := api.Explicit(ctx, a.method, a.target, body)
	if err != nil {
		if response.Status != 0 {
			_ = json.NewEncoder(c.Out).Encode(map[string]any{"status": response.Status, "body": nil, "bodyType": "unavailable"})
		}
		return fail(err)
	}
	if err := writeResponse(c.Out, response, token); err != nil {
		return fail(cause.New("API応答を出力できません", err))
	}
	if response.Status < 200 || response.Status >= 300 {
		return 1
	}
	return 0
}

var tokenPattern = regexp.MustCompile(`\bid[kh]_[A-Za-z0-9_-]+`)
var bearerPattern = regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/=-]+`)

func scrubText(s, token string) (string, bool) {
	out := strings.ReplaceAll(s, token, "[REDACTED]")
	out = tokenPattern.ReplaceAllString(out, "[REDACTED]")
	// Provider keys (sk-…) and GitHub tokens in any field are hidden as well.
	out = redact.Known(out)
	out = bearerPattern.ReplaceAllString(out, "Bearer [REDACTED]")
	return out, out != s
}
func sensitive(key string) bool {
	key = strings.ToLower(key)
	// The Claude API key is returned as "key"; any field named key is a secret.
	if key == "key" || strings.HasSuffix(key, "privatekey") || strings.HasSuffix(key, "private_key") {
		return true
	}
	for _, suffix := range []string{"file", "sha256", "count", "kind", "name", "type", "valid"} {
		if strings.HasSuffix(key, suffix) {
			return false
		}
	}
	for _, part := range []string{"token", "password", "secret", "authorization", "credential", "cookie", "csrf", "apikey", "api_key"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}
func scrub(value any, token string) (any, bool) {
	switch v := value.(type) {
	case string:
		return scrubText(v, token)
	case []any:
		changed := false
		for i, x := range v {
			item, redacted := scrub(x, token)
			v[i] = item
			changed = changed || redacted
		}
		return v, changed
	case map[string]any:
		changed := false
		clean := make(map[string]any, len(v))
		for k, x := range v {
			key, keyChanged := scrubText(k, token)
			changed = changed || keyChanged
			if sensitive(k) && x != nil && x != "" {
				clean[key] = "[REDACTED]"
				changed = true
			} else {
				item, redacted := scrub(x, token)
				clean[key] = item
				changed = changed || redacted
			}
		}
		return clean, changed
	default:
		return value, false
	}
}
func writeResponse(out io.Writer, response intake.Response, token string) error {
	var body any
	bodyType := "empty"
	redacted := false
	if len(response.Body) > 0 {
		dec := json.NewDecoder(bytes.NewReader(response.Body))
		dec.UseNumber()
		if json.Valid(response.Body) && dec.Decode(&body) == nil {
			bodyType = "json"
			body, redacted = scrub(body, token)
		} else {
			bodyType = "text"
			body, redacted = scrubText(string(response.Body), token)
		}
	}
	return json.NewEncoder(out).Encode(map[string]any{"status": response.Status, "body": body, "bodyType": bodyType, "redacted": redacted})
}

package providers

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"issue-desk-worker/internal/cause"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"issue-desk-worker/internal/process"
)

type Settings struct{ Provider, Model, Effort string }
type Request struct {
	Settings                                        Settings
	Policy                                          Policy
	Codex, Claude, Antigravity, AntigravitySettings string
	Prompt, Session                                 string
	APIKey                                          string
	Review                                          bool
	ReportOnly                                      bool
	Timeout                                         time.Duration
	OnSession                                       func(string) error
}

// ReportOnlyError distinguishes an unusable supplemented report/conversation
// from transport, authentication, cancellation, or provider quota failures.
type ReportOnlyError struct{ Err error }

func (e *ReportOnlyError) Error() string { return e.Err.Error() }
func (e *ReportOnlyError) Unwrap() error { return e.Err }

type Usage struct {
	Input, Output, Cached, CacheWrite           *int64
	Calls, ContextMaxTokens, ContextTotalTokens *int64
	CostUSD                                     *float64
}
type Answer struct {
	// APIKeyMode means Claude started with an explicitly supplied child API key.
	// It records authentication mode, not successful billing or a billed amount.
	APIKeyMode bool
	Transient  bool
	Denied     []json.RawMessage
	JSON       json.RawMessage
	Usage      Usage
	Limits     []LimitReading
	Elapsed    time.Duration
	Session    string
	Outcome    string
}
type UsageLimit struct {
	Provider  string
	Until     time.Time
	Source    string
	LimitKind string
	APIKey    bool
}

func (e *UsageLimit) Error() string {
	return "実行先の利用上限に達しました: " + e.Provider
}

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$`)

func UUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	const hex = "0123456789abcdef"
	var s strings.Builder
	for i, v := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			s.WriteByte('-')
		}
		s.WriteByte(hex[v>>4])
		s.WriteByte(hex[v&15])
	}
	return s.String()
}
func preflightAuth(ctx context.Context, codex, claude string) error {
	result, err := process.Run(ctx, codex, []string{"login", "status"}, process.Options{Timeout: 20 * time.Second})
	if err != nil || result.Code != 0 || !strings.Contains(result.Out+result.Err, "ChatGPT") {
		return checkFailure("CodexのChatGPT認証を確認できません", err, result.Code, "ChatGPTでのログインと表示されません")
	}
	if claude != "" {
		result, err = process.Run(ctx, claude, []string{"auth", "status", "--json"}, process.Options{Timeout: 20 * time.Second})
		if err != nil || result.Code != 0 || !claudeSubscription(result.Out) {
			return checkFailure("Claudeの契約アカウントの認証を確認できません", err, result.Code, "契約アカウントでのログインと表示されません")
		}
	}
	return nil
}
func Call(ctx context.Context, r Request) (answer Answer, callErr error) {
	if r.APIKey != "" {
		defer func() { answer.JSON, callErr = redactCallResult(answer.JSON, callErr, r.APIKey) }()
	}
	if err := checkCall(r); err != nil {
		return answer, err
	}
	dir := filepath.Join(r.Policy.Root, "control", "results")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return answer, err
	}
	nonce := UUID()
	schema := filepath.Join(dir, nonce+".schema.json")
	output := filepath.Join(dir, nonce+".answer.json")
	if err := os.WriteFile(schema, Schema(r.Review), 0600); err != nil {
		return answer, err
	}
	defer os.Remove(schema)
	defer os.Remove(output)
	command, err := commandFor(r, schema, output)
	if err != nil {
		return answer, err
	}
	stream := &callStream{r: r, answer: &answer, session: command.session}
	input := r.Prompt
	if r.Settings.Provider == "antigravity" {
		input = ""
	}
	started := time.Now()
	var usageBaseline codexUsageBaseline
	if r.Settings.Provider == "codex" && r.Session != "" {
		usageBaseline = snapshotCodexUsage(codexHome(), r.Session, started)
	}
	// Processes the AI started in the background end with the provider run.
	result, err := process.Run(ctx, command.exe, command.args, process.Options{Dir: r.Policy.Tree, Input: input, Timeout: r.Timeout, Env: command.env, OnLine: stream.onLine, KillGroupOnExit: true})
	answer.APIKeyMode = command.apiKeyMode && result.Started
	if r.Settings.Provider == "codex" {
		readCodexUsage(&answer, r, stream.session, started, usageBaseline)
	}
	if r.Settings.Provider == "antigravity" {
		antigravityMetadata(antigravityOutput(result.Out), &answer)
	}
	answer.Transient = err == nil && transientFailure(r.Settings.Provider, result)
	answer.Elapsed = result.Elapsed
	answer.Session = stream.session
	answer.Outcome = "completed"
	if err := callFailure(&answer, r, result, err, stream.callbackErr); err != nil {
		return answer, err
	}
	answerData := stream.answerData
	if r.Settings.Provider == "codex" {
		answerData, err = os.ReadFile(output)
		if err != nil {
			return answer, cause.New("AIの構造化結果ファイルがありません", err)
		}
	}
	if r.Settings.Provider == "antigravity" {
		answerData, err = decodeAntigravity(result.Out, &answer)
		if err != nil {
			return answer, err
		}
	}

	if err := Validate(answerData, r.Review); err != nil {
		if r.ReportOnly {
			return answer, &ReportOnlyError{Err: err}
		}
		return answer, err
	}
	answer.JSON = answerData
	return answer, nil
}

// CheckReadOnlyProvider verifies that the selected CLI enforces Policy.ReadOnly.
func CheckReadOnlyProvider(provider string) error {
	switch provider {
	case "codex", "claude":
		return nil
	case "antigravity":
		return errors.New("Antigravityでは報告専用処理に必要な読み取り専用制約を保証できません")
	default:
		return errors.New("この実行先では読み取り専用制約を保証できません")
	}
}

// checkCall refuses a report-only call without its safeguards and an invalid
// conversation ID.
func checkCall(r Request) error {
	if r.ReportOnly {
		if r.Review || !r.Policy.ReadOnly || !sessionPattern.MatchString(r.Session) {
			return errors.New("報告補完には同じ実装会話と読取専用制約が必要です")
		}
	}
	if r.Policy.ReadOnly {
		if err := CheckReadOnlyProvider(r.Settings.Provider); err != nil {
			return err
		}
	}
	if r.Session != "" && !sessionPattern.MatchString(r.Session) {
		return errors.New("AI会話IDが不正です")
	}
	return nil
}

// callCommand is the provider CLI invocation of one call.
type callCommand struct {
	exe        string
	args       []string
	env        map[string]string
	apiKeyMode bool
	// session is the conversation the call resumes or starts, if known.
	session string
}

// commandFor builds the provider's command line. A new Claude conversation is
// reported to OnSession before the call starts.
func commandFor(r Request, schema, output string) (callCommand, error) {
	c := callCommand{env: map[string]string{}, session: r.Session}
	if r.Policy.Temp != "" {
		c.env = ToolchainEnv(r.Policy.Temp)
		for _, key := range []string{"TEMP", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME"} {
			c.env[key] = r.Policy.Temp
		}
	}
	switch r.Settings.Provider {
	case "codex":
		c.exe = r.Codex
		c.args = []string{"exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check"}
		c.args = append(c.args, r.Policy.Args()...)
		c.args = append(c.args, "-m", r.Settings.Model, "-c", "model_reasoning_effort="+quoted(r.Settings.Effort), "--json", "--output-schema", schema, "-o", output, "-C", r.Policy.Tree)
		if c.session != "" {
			c.args = append(c.args, "resume", c.session, "-")
		} else {
			c.args = append(c.args, "-")
		}
	case "claude":
		c.exe = r.Claude
		if r.APIKey != "" {
			c.env["ANTHROPIC_API_KEY"] = r.APIKey
			c.apiKeyMode = true
		}
		if c.exe == "" {
			return c, errors.New("Claudeが設定されていません")
		}
		tools := "Read,Edit,Write,Glob,Grep"
		if r.Review || r.Policy.ReadOnly {
			tools = "Read,Glob,Grep"
		}
		c.args = []string{"-p", "--model", r.Settings.Model, "--effort", r.Settings.Effort, "--tools", tools, "--restricted", "--permission-mode", "dontAsk", "--setting-sources", "", "--settings", r.Policy.ClaudeSettings(), "--strict-mcp-config", "--disable-slash-commands", "--no-chrome", "--output-format", "stream-json", "--verbose", "--json-schema", string(Schema(r.Review))}
		if r.Review || (r.Policy.ReadOnly && c.session == "") {
			c.args = append(c.args, "--no-session-persistence")
		} else if c.session != "" {
			c.args = append(c.args, "--resume", c.session)
		} else {
			c.session = UUID()
			c.args = append(c.args, "--session-id", c.session)
			if r.OnSession != nil {
				if err := r.OnSession(c.session); err != nil {
					return c, err
				}
			}
		}
		for _, file := range r.Policy.ReadFiles {
			c.args = append(c.args, "--add-dir", filepath.Dir(file))
		}
	case "antigravity":
		if r.Review {
			return c, errors.New("Antigravityは独立レビューを担当しません")
		}
		if err := checkAntigravity(r); err != nil {
			return c, err
		}
		c.exe = r.Antigravity
		c.args = []string{"-p", r.Prompt + "\n回答契約(JSON): " + string(Schema(false)), "--model", r.Settings.Model, "--mode", "accept-edits", "--output-format", "json"}
		if c.session != "" {
			c.args = append(c.args, "--conversation", c.session)
		}
	default:
		return c, errors.New("AI実行先が不正です")
	}
	return c, nil
}

// callStream reads the provider's output lines: usage, limits, the
// conversation ID and the answer.
type callStream struct {
	r           Request
	answer      *Answer
	session     string
	answerData  json.RawMessage
	callbackErr error
}

func (s *callStream) onLine(line string) {
	r, answer := s.r, s.answer
	var event map[string]json.RawMessage
	if json.Unmarshal([]byte(line), &event) != nil {
		return
	}
	if raw := event["usage"]; len(raw) > 0 {
		u := readUsage(raw, r.Settings.Provider)
		if r.Settings.Provider == "codex" && r.Session != "" {
			u = codexCumulativeUsage(raw)
		}
		u.Calls = answer.Usage.Calls
		u.CostUSD = answer.Usage.CostUSD
		answer.Usage = u
	}
	if r.Settings.Provider == "claude" && r.APIKey == "" {
		answer.Limits = append(answer.Limits, ClaudeLimits(line, time.Now())...)
	}
	if r.ReportOnly && r.Settings.Provider == "claude" {
		var id string
		_ = json.Unmarshal(event["session_id"], &id)
		if id != "" && id != r.Session {
			s.callbackErr = &ReportOnlyError{Err: errors.New("報告補完の会話IDが変わりました")}
			return
		}
	}
	var kind string
	_ = json.Unmarshal(event["type"], &kind)
	if r.Settings.Provider == "codex" && kind == "turn.completed" {
		if r.Session != "" && len(event["usage"]) == 0 {
			answer.Usage.Input, answer.Usage.Cached, answer.Usage.Output = nil, nil, nil
		}
		if answer.Usage.Calls == nil {
			zero := int64(0)
			answer.Usage.Calls = &zero
		}
		*answer.Usage.Calls++
	}
	if r.Settings.Provider == "claude" && kind == "result" {
		answer.Usage.Calls = nonnegativeInt(event["num_turns"])
		answer.Usage.CostUSD = parseCostUSD(event["total_cost_usd"])
	}
	if kind == "thread.started" {
		var id string
		_ = json.Unmarshal(event["thread_id"], &id)
		if sessionPattern.MatchString(id) {
			if r.ReportOnly && id != r.Session {
				s.callbackErr = &ReportOnlyError{Err: errors.New("報告補完の会話IDが変わりました")}
				return
			}
			s.session = id
			if r.OnSession != nil && s.callbackErr == nil {
				s.callbackErr = r.OnSession(id)
			}
		}
	}
	if kind == "result" {
		if raw := event["structured_output"]; len(raw) > 0 && string(raw) != "null" {
			s.answerData = raw
		} else {
			var text string
			_ = json.Unmarshal(event["result"], &text)
			s.answerData = []byte(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```"), "```")))
		}
	}
}

// readCodexUsage fills the limits, context usage and token usage of a Codex
// call from the Codex home, where the stream did not report them.
func readCodexUsage(answer *Answer, r Request, session string, started time.Time, usageBaseline codexUsageBaseline) {
	answer.Limits = CodexLimits(codexHome(), session, started, time.Now())
	contextUsage := codexContextUsage(codexHome(), session, started, r.Session != "", usageBaseline)
	if contextUsage.Calls != nil {
		answer.Usage.ContextMaxTokens, answer.Usage.ContextTotalTokens, answer.Usage.Calls = contextUsage.ContextMaxTokens, contextUsage.ContextTotalTokens, contextUsage.Calls
	}
	if r.Session != "" {
		u := resumedCodexUsage(codexHome(), session, started, usageBaseline, answer.Usage)
		answer.Usage.Input, answer.Usage.Cached, answer.Usage.Output = u.Input, u.Cached, u.Output
	} else if answer.Usage.Input == nil && answer.Usage.Output == nil && answer.Usage.Cached == nil && answer.Usage.CacheWrite == nil {
		u := recoverCodexUsage(codexHome(), session, started, r.Session != "", usageBaseline)
		answer.Usage.Input, answer.Usage.Cached, answer.Usage.Output = u.Input, u.Cached, u.Output
	}
}

// callFailure classifies a finished run: a process error, a refused stream
// callback, a usage limit or a failed exit. It returns nil for a successful
// run, whose Codex call count it then fills.
func callFailure(answer *Answer, r Request, result process.Result, err, callbackErr error) error {
	if err != nil {
		answer.Outcome = "failed"
		if errors.Is(err, context.DeadlineExceeded) {
			answer.Outcome = "timeout"
		}
		if errors.Is(err, context.Canceled) {
			answer.Outcome = "aborted"
		}
		return err
	}
	if callbackErr != nil {
		answer.Transient = false
		return callbackErr
	}
	if limited := usageFailure(r.Settings.Provider, result, time.Now()); limited != nil {
		answer.Outcome = "failed"
		limited.APIKey = r.APIKey != ""
		return limited
	}
	if result.Code != 0 {
		answer.Outcome = "failed"
		if r.APIKey != "" {
			result.Out = strings.ReplaceAll(result.Out, r.APIKey, "[redacted]")
			result.Err = strings.ReplaceAll(result.Err, r.APIKey, "[redacted]")
		}
		return errors.New(exitMessage(result))
	}
	if r.Settings.Provider == "codex" && answer.Usage.Calls == nil {
		zero := int64(0)
		answer.Usage.Calls = &zero
	}
	return nil
}
func readUsage(raw json.RawMessage, provider string) Usage {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	read := func(keys ...string) *int64 {
		for _, key := range keys {
			if n := nonnegativeInt(m[key]); n != nil {
				return n
			}
		}
		return nil
	}
	u := Usage{Input: read("input_tokens", "inputTokens", "prompt_tokens", "promptTokenCount"), Output: read("output_tokens", "outputTokens", "completion_tokens", "candidatesTokenCount"), Cached: read("cached_input_tokens", "cache_read_input_tokens", "cacheReadTokens", "cachedTokens", "cachedContentTokenCount"), CacheWrite: read("cache_creation_input_tokens", "cacheWriteTokens")}
	return u
}
func nonnegativeInt(raw json.RawMessage) *int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var n int64
	if json.Unmarshal(raw, &n) != nil || n < 0 {
		return nil
	}
	return &n
}
func parseCostUSD(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil || !ValidCostUSD(&n) {
		return nil
	}
	return &n
}
func checkAntigravity(r Request) error {
	data, err := os.ReadFile(r.AntigravitySettings)
	if err != nil {
		return cause.New("Antigravityの保護設定を読めません", err)
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		return errors.New("Antigravityの保護設定が不正です")
	}
	if cfg["allowNonWorkspaceAccess"] != false || cfg["enableTerminalSandbox"] != true || cfg["useG1Credits"] != false {
		return errors.New("Antigravityの作業領域・シェル・課金の保護設定を確認できません")
	}
	var validate func(any) bool
	validate = func(v any) bool {
		switch value := v.(type) {
		case map[string]any:
			for key, x := range value {
				if (key == "allowNonWorkspaceAccess" || key == "useG1Credits") && x != false {
					return false
				}
				if key == "enableTerminalSandbox" && x != true {
					return false
				}
				if !validate(x) {
					return false
				}
			}
		case []any:
			for _, x := range value {
				if !validate(x) {
					return false
				}
			}
		}
		return true
	}
	if !validate(cfg) {
		return errors.New("Antigravityに矛盾する入れ子の保護設定があります")
	}
	trusted, _ := cfg["trustedWorkspaces"].([]any)
	workspace := filepath.Join(r.Policy.Root, "workspaces")
	found := false
	for _, raw := range trusted {
		if value, ok := raw.(string); ok && filepath.Clean(value) == workspace {
			found = true
		}
	}
	if !found {
		return errors.New("Antigravityの信頼済み作業領域がありません")
	}
	permissions, _ := cfg["permissions"].(map[string]any)
	rules, _ := permissions["deny"].([]any)
	have := map[string]bool{}
	for _, raw := range rules {
		if value, ok := raw.(string); ok {
			have[value] = true
		}
	}
	if !have["RunCommand"] || !have["Bash"] {
		return errors.New("Antigravityのシェル拒否設定がありません")
	}
	for _, dir := range protected(r.Policy) {
		for _, tool := range []string{"Read", "Edit", "Write"} {
			if !have[tool+"("+filepath.ToSlash(dir)+"/**)"] && !have[tool+"(/"+filepath.ToSlash(dir)+"/**)"] {
				return errors.New("Antigravityの資格情報保護が不足しています")
			}
		}
	}
	return nil
}

var secretLike = regexp.MustCompile(`[A-Za-z0-9_\-]{32,}`)

// exitMessage は、AIの非ゼロ終了を、終了コードと出力の抜粋つきで報告する。
// 抜粋が無いと、起動直後の失敗(認証・引数・モデル名など)の原因を、受付からも利用者からも特定できない。
// 終了時に毎回出るだけの雑音(古いセッションの記録の警告)は除き、先頭と末尾を載せる。鍵に見える長い文字列は伏せる。
func exitMessage(r process.Result) string {
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(r.Err), "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" || strings.Contains(line, "stale rollout path") {
			continue
		}
		lines = append(lines, secretLike.ReplaceAllString(line, "…"))
	}
	// Codex は失敗の理由を標準出力のJSONイベント(error・turn.failed)にも出す。標準エラーに無くてもここから拾う。
	for _, line := range strings.Split(r.Out, "\n") {
		if strings.Contains(line, `"type":"error"`) || strings.Contains(line, "turn.failed") || strings.Contains(line, `"type": "error"`) {
			lines = append(lines, secretLike.ReplaceAllString(strings.Join(strings.Fields(line), " "), "…"))
		}
	}
	if len(lines) == 0 {
		if out := strings.Split(strings.TrimSpace(r.Out), "\n"); out[len(out)-1] != "" {
			lines = append(lines, secretLike.ReplaceAllString(strings.Join(strings.Fields(out[len(out)-1]), " "), "…"))
		}
	}
	excerpt := strings.Join(lines, " / ")
	runes := []rune(excerpt)
	if len(runes) > 700 {
		excerpt = string(runes[:250]) + " … " + string(runes[len(runes)-450:])
	}
	message := "AIが非ゼロで終了しました（終了コード" + strconv.Itoa(r.Code) + "）"
	if excerpt != "" {
		message += "。出力の抜粋: " + excerpt
	}
	return message
}

// Package fleetconfig decodes the fleet configuration the intake distributes
// and answers what it means for one host: its repositories, providers,
// capacity and test-only assignment.
package fleetconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"issue-desk-worker/internal/cause"
	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/webhook"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Repository struct {
	NativeNetwork      bool     `json:"nativeNetwork,omitempty"`
	NativeToolRoots    []string `json:"nativeToolRoots,omitempty"`
	NativeTestHost     string   `json:"nativeTestHost,omitempty"`
	Provider           string   `json:"provider"`
	Name               string   `json:"name"`
	AutoMerge          bool     `json:"autoMerge"`
	TestCommand        []string `json:"testCommand"`
	SetupCommand       []string `json:"setupCommand"`
	TestTimeoutSeconds int      `json:"testTimeoutSeconds"`
}
type Model struct {
	Model       string `json:"model"`
	ClaudeModel string `json:"claudeModel"`
	Effort      string `json:"reasoningEffort"`
}
type RepositoryReleasePolicy struct {
	PermissionFiles []string `json:"permissionFiles"`
}
type ReleasePolicy struct {
	PermissionFiles []string                           `json:"permissionFiles"`
	Repositories    map[string]RepositoryReleasePolicy `json:"repositories,omitempty"`
}
type Fleet struct {
	HostRole           string                 `json:"hostRole"`
	TestRepositories   []string               `json:"testRepositories"`
	NativeTestVersions map[string]string      `json:"nativeTestVersions"`
	Provider           string                 `json:"provider"`
	Webhook            *webhook.Configuration `json:"webhook"`
	WebhookVerifier    string                 `json:"webhookVerifier"`
	GiteaURL           string                 `json:"giteaUrl"`
	WorkerTokenFile    string                 `json:"workerTokenFile"`
	TokenFile          string                 `json:"tokenFile"`
	RuntimeDir         string                 `json:"runtimeDir"`
	GitUser            string                 `json:"gitUser"`
	Git                string                 `json:"git"`
	Codex              string                 `json:"codex"`
	Claude             string                 `json:"claude"`
	Node               string                 `json:"node"`
	PNPM               string                 `json:"pnpm"`
	Python             string                 `json:"python"`
	Model              string                 `json:"model"`
	ClaudeModel        string                 `json:"claudeModel"`
	Effort             string                 `json:"reasoningEffort"`
	Equivalents        map[string]string      `json:"codexEquivalents"`
	JobTimeoutSeconds  int                    `json:"jobTimeoutSeconds"`
	TestTimeoutSeconds int                    `json:"testTimeoutSeconds"`
	Repositories       []Repository           `json:"repositories"`
	Implementation     struct {
		Tiers map[string]Model `json:"tiers"`
	} `json:"implementation"`
	Review struct {
		ClaudeModel         string `json:"claudeModel"`
		AntigravityReviewer string `json:"antigravityReviewer"`
		Tiers               map[string]struct {
			ClaudeModel string `json:"claudeModel"`
		} `json:"tiers"`
	} `json:"review"`
	Assignments struct {
		HostRoles                map[string]string         `json:"hostRoles"`
		TestRepositories         map[string][]string       `json:"testRepositories"`
		PollSeconds              int                       `json:"pollSeconds"`
		WorkerAccounts           map[string]string         `json:"workerAccounts"`
		Implementers             map[string][]string       `json:"implementers"`
		ImplementationRunLimit   int                       `json:"implementationRunLimit"`
		ImplementationUsageLimit int64                     `json:"implementationUsageLimit"`
		Hosts                    json.RawMessage           `json:"hosts"`
		HostLimits               map[string]int            `json:"hostLimits"`
		HostRepositoryLimits     map[string]map[string]int `json:"hostRepositoryLimits"`
	} `json:"assignments"`
	ReleasePolicy *ReleasePolicy `json:"releasePolicy"`
	Antigravity   *struct {
		Command      string `json:"command"`
		SettingsFile string `json:"settingsFile"`
		Model        string `json:"model"`
	} `json:"antigravity"`
}
type Setting struct {
	Value json.RawMessage `json:"value"`
	Until time.Time       `json:"until"`
}

// Configuration is the fleet configuration as seen from one host (Host).
type Configuration struct {
	// AssignmentHost is the host whose own assignment fields (HostRole,
	// TestRepositories) apply; empty means Host.
	AssignmentHost string `json:"-"`
	// SettingsVersion is the version of the shared settings snapshot.
	SettingsVersion string `json:"-"`
	Fleet
	Host        string
	Settings    map[string]Setting
	Version     string
	ReviewNotes json.RawMessage
}

func Decode(snapshot intake.ConfigSnapshot, host string) (Configuration, error) {
	var out Configuration
	if err := json.Unmarshal(snapshot.Config, &out.Fleet); err != nil {
		return out, cause.New("実行設定のJSONが不正です", err)
	}
	if hasJSON(snapshot.Settings) {
		if err := json.Unmarshal(snapshot.Settings, &out.Settings); err != nil {
			return out, cause.New("運用設定のJSONが不正です", err)
		}
	}
	if out.Claude == "" {
		out.Claude = os.Getenv("ISSUE_DESK_CLAUDE")
		if out.Claude == "" {
			out.Claude, _ = exec.LookPath("claude")
		}
	}
	if out.Assignments.PollSeconds == 0 {
		out.Assignments.PollSeconds = 300
	}
	if out.Assignments.PollSeconds < 1 || out.Assignments.PollSeconds > 86400 {
		return out, errors.New("pollSecondsが範囲外です")
	}
	out.Host = host
	if role := out.HostAssignment().Role; !validHostRole(role) {
		// A misspelt test-only must not make this host take implementation jobs.
		return out, fmt.Errorf("host roleが不正です: %q（implementation か test-only にしてください）", role)
	}
	if out.TestOnly() {
		return out, errors.New("test-onlyホストはworker appleを使用してください")
	}
	out.Version = snapshot.ConfigurationVersion
	out.ReviewNotes = snapshot.ReviewNotes
	if out.Git == "" || out.Codex == "" || out.GiteaURL == "" || !filepath.IsAbs(out.RuntimeDir) || out.WorkerTokenFile == "" {
		return out, errors.New("実行設定の必須項目が足りません")
	}
	if out.WorkerTokenFile == out.TokenFile && out.TokenFile != "" {
		return out, errors.New("監視用トークンに管理トークンは使えません")
	}
	if out.JobTimeoutSeconds <= 0 {
		out.JobTimeoutSeconds = 1800
	}
	if out.TestTimeoutSeconds <= 0 {
		out.TestTimeoutSeconds = 900
	}
	if out.Assignments.ImplementationRunLimit == 0 {
		out.Assignments.ImplementationRunLimit = 4
	}
	if out.Assignments.ImplementationUsageLimit == 0 {
		out.Assignments.ImplementationUsageLimit = 8000000
	}
	if out.Assignments.ImplementationRunLimit < 1 || out.Assignments.ImplementationRunLimit > 100 || out.Assignments.ImplementationUsageLimit < 1 {
		return out, errors.New("実行上限の設定が不正です")
	}
	return out, nil
}
func (c Configuration) Repository(name string) (Repository, error) {
	for _, r := range c.Repositories {
		if r.Name == name {
			if len(r.TestCommand) == 0 {
				return r, errors.New("固定テストが設定されていません")
			}
			return r, nil
		}
	}
	return Repository{}, errors.New("このPCの担当にないリポジトリです")
}
func (c Configuration) SettingsFor(provider, tier string, review bool) (string, string, string, error) {
	if provider != "codex" && provider != "claude" && provider != "antigravity" {
		return "", "", "", errors.New("実行先が不正です")
	}
	if review {
		if provider == "antigravity" && c.Review.AntigravityReviewer == "claude" {
			provider = "codex"
		}
		if provider == "codex" {
			provider = "claude"
		} else {
			provider = "codex"
		}
	}
	// An explicit temporary disable moves the affected work to the other configured provider.
	setting, configured := c.Settings["providerDisable."+provider]
	var disabled struct {
		Until time.Time `json:"until"`
	}
	_ = json.Unmarshal(setting.Value, &disabled)
	if configured && (disabled.Until.After(time.Now()) || setting.Until.After(time.Now())) {
		if provider == "codex" {
			provider = "claude"
		} else if provider == "claude" {
			provider = "codex"
		}
	}
	effort := c.Effort
	if effort == "" {
		effort = "medium"
	}
	model := c.Model
	if provider == "claude" {
		model = c.ClaudeModel
		if model == "" {
			model = "opus"
		}
	} else if v := c.Equivalents[c.ClaudeModel]; v != "" {
		model = v
	}
	if review {
		reviewModel := c.Review.ClaudeModel
		if tier == "light" || tier == "standard" || tier == "heavy" {
			if v := c.Review.Tiers[tier].ClaudeModel; v != "" {
				reviewModel = v
			}
		}
		if reviewModel != "" {
			if provider == "claude" {
				model = reviewModel
			} else if value := c.Equivalents[reviewModel]; value != "" {
				model = value
			}
		}
	} else if entry, ok := c.Implementation.Tiers[tier]; ok {
		if provider == "claude" && entry.ClaudeModel != "" {
			model = entry.ClaudeModel
		} else if provider == "codex" && entry.Model != "" {
			model = entry.Model
		}
		if entry.Effort != "" {
			effort = entry.Effort
		}
	}
	if provider == "antigravity" {
		if c.Antigravity == nil {
			return "", "", "", errors.New("Antigravityが設定されていません")
		}
		model = c.Antigravity.Model
	}
	if model == "" {
		return "", "", "", errors.New("実行モデルが設定されていません")
	}
	return provider, model, effort, nil
}

// ReviewNotesFor returns bounded data for exactly one repository. Invalid input is ignored.
func (c Configuration) ReviewNotesFor(repo string) []ReviewNote {
	var repos map[string]json.RawMessage
	if json.Unmarshal(c.ReviewNotes, &repos) != nil || repos == nil {
		return nil
	}
	var input []struct {
		Kind *string `json:"kind"`
		Text *string `json:"text"`
	}
	if json.Unmarshal(repos[repo], &input) != nil {
		return nil
	}
	for _, item := range input {
		if item.Kind == nil || item.Text == nil || *item.Kind == "" || *item.Text == "" {
			return nil
		}
	}
	const perItem, total = 1000, 4000
	remaining := total
	var notes []ReviewNote
	for _, item := range input {
		kind, text := []rune(*item.Kind), []rune(*item.Text)
		limit := perItem
		if remaining < limit {
			limit = remaining
		}
		if len(kind) > limit {
			kind = kind[:limit]
		}
		limit -= len(kind)
		if len(text) > limit {
			text = text[:limit]
		}
		if len(text) == 0 {
			break
		}
		notes = append(notes, ReviewNote{Kind: string(kind), Text: string(text)})
		remaining -= len(kind) + len(text)
		if remaining == 0 {
			break
		}
	}
	return notes
}

type ReviewNote struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

func hasJSON(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value != "" && value != "null"
}

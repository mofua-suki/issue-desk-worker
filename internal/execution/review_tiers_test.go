package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"issue-desk-worker/internal/intake"
	"issue-desk-worker/internal/jobspec"
	"issue-desk-worker/internal/journal"
	"issue-desk-worker/internal/providers"
)

func TestReviewTierSelection(t *testing.T) {
	for _, tiers := range []string{``, `,"tiers":{}`, `,"tiers":{"light":{"claudeModel":"small"},"standard":{},"heavy":{"claudeModel":"large"},"unknown":{"claudeModel":"ignored"}}`} {
		for _, tier := range []string{"light", "standard", "heavy", "", "unknown"} {
			for _, base := range []string{"codex", "claude"} {
				for _, disabled := range []bool{false, true} {
					for _, mapped := range []bool{false, true} {
						name := fmt.Sprintf("%s/%s/%s/disable=%t/mapped=%t", tiers, tier, base, disabled, mapped)
						t.Run(name, func(t *testing.T) {
							equivalents := `{"base":"base-codex","review":"review-codex"}`
							if mapped {
								equivalents = `{"base":"base-codex","review":"review-codex","small":"small-codex","large":"large-codex"}`
							}
							raw := fmt.Sprintf(`{"git":"git","codex":"codex","giteaUrl":"https://example.invalid","runtimeDir":"/tmp/runtime","workerTokenFile":"token","model":"default-codex","claudeModel":"base","reasoningEffort":"high","codexEquivalents":%s,"implementation":{"tiers":{"light":{"claudeModel":"implementation","model":"impl-codex","reasoningEffort":"low"}}},"review":{"claudeModel":"review"%s}}`, equivalents, tiers)
							c, err := DecodeConfig(intake.ConfigSnapshot{Config: json.RawMessage(raw)}, "host")
							if err != nil {
								t.Fatal(err)
							}
							actual := "claude"
							if base == "claude" {
								actual = "codex"
							}
							if disabled {
								c.Settings = map[string]Setting{"providerDisable." + actual: {Value: json.RawMessage(`{"until":"2099-01-01T00:00:00Z"}`)}}
								if actual == "claude" {
									actual = "codex"
								} else {
									actual = "claude"
								}
							}
							selected := "review"
							if tiers != "" && tiers != `,"tiers":{}` {
								if tier == "light" {
									selected = "small"
								}
								if tier == "heavy" {
									selected = "large"
								}
							}
							want := selected
							if actual == "codex" {
								want = "base-codex"
								if selected == "review" {
									want = "review-codex"
								} else if mapped {
									want = selected + "-codex"
								}
							}
							p, model, effort, err := c.SettingsFor(base, tier, true)
							if err != nil || p != actual || model != want || effort != "high" {
								t.Fatalf("review got %s %s %s %v, want %s %s high", p, model, effort, err, actual, want)
							}
							// Implementation selection uses the implementation tier across review configurations.
							implementationModel, implementationEffort := "base-codex", "high"
							if base == "claude" {
								implementationModel = "base"
							}
							if tier == "light" {
								implementationModel, implementationEffort = "impl-codex", "low"
								if base == "claude" {
									implementationModel = "implementation"
								}
							}
							p, model, effort, err = c.SettingsFor(base, tier, false)
							if err != nil || p != base || model != implementationModel || effort != implementationEffort {
								t.Fatalf("implementation got %s %s %s %v, want %s %s %s", p, model, effort, err, base, implementationModel, implementationEffort)
							}
						})
					}
				}
			}
		}
	}
}

func TestReviewTierDefaultsAndFallback(t *testing.T) {
	var c Configuration
	c.Model = "codex-default"
	c.Review.Tiers = map[string]struct {
		ClaudeModel string `json:"claudeModel"`
	}{"light": {ClaudeModel: "unmapped"}}
	for _, tc := range []struct{ base, tier, model string }{{"codex", "heavy", "opus"}, {"codex", "light", "unmapped"}, {"claude", "light", "codex-default"}, {"claude", "heavy", "codex-default"}} {
		_, model, effort, err := c.SettingsFor(tc.base, tc.tier, true)
		if err != nil || model != tc.model || effort != "medium" {
			t.Fatalf("%+v: %s %s %v", tc, model, effort, err)
		}
	}
}

func TestReviewTierCallReachesSavedUsage(t *testing.T) {
	f := newPipeline(t, "approve")
	c := &f.Executor.Config
	c.Review.Tiers = map[string]struct {
		ClaudeModel string `json:"claudeModel"`
	}{"light": {ClaudeModel: "small"}}
	c.Equivalents = map[string]string{"small": "small-codex"}
	for _, base := range []string{"codex", "claude"} {
		d := Descriptor{Descriptor: jobspec.Descriptor{ID: 1, Repo: "owner/project", Issue: 7, Tier: "light"}}
		r := &journal.Record{ID: 1, Repo: d.Repo, Issue: d.Issue, Provider: base, Tree: f.root, Runs: 1}
		var sent providers.Settings
		f.Executor.Call = func(_ context.Context, req providers.Request) (providers.Answer, error) {
			if !req.Review {
				t.Fatal("not review")
			}
			sent = req.Settings
			return providers.Answer{}, nil
		}
		if _, err := f.Executor.callAIOnce(context.Background(), d, r, "review", true, nil); err != nil {
			t.Fatal(err)
		}
		saved, err := f.Executor.Store.Load(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal(saved.Usage[len(saved.Usage)-1], &row); err != nil {
			t.Fatal(err)
		}
		want := "small"
		if base == "claude" {
			want = "small-codex"
		}
		if sent.Model != want || row["model"] != sent.Model || row["provider"] != sent.Provider || row["tier"] != d.Tier || row["step"] != "独立レビュー" {
			t.Fatalf("sent %+v saved %v", sent, row)
		}
	}
}

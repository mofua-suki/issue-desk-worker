package fleetconfig

import (
	"encoding/json"
	"testing"
)

func TestNormalizedAssignmentLimitsRemainUnchanged(t *testing.T) {
	var c Configuration
	raw := []byte(`{"assignments":{"hosts":["nas","x1"],"hostLimits":{"x1":4},"hostRepositoryLimits":{"x1":{"Owner/Project":2}},"implementers":{"Owner/Project":["x1"]}},"repositories":[{"name":"Owner/Project"}]}`)
	if err := json.Unmarshal(raw, &c.Fleet); err != nil {
		t.Fatal(err)
	}
	c.Host = "x1"
	if c.Capacity() != 4 || c.RepoLimits()["Owner/Project"] != 2 {
		t.Fatal("配信された設定を狭めました")
	}
}

func TestProviderDefaultsPreserveUnmarkedExistingJobs(t *testing.T) {
	c := Configuration{Fleet: Fleet{Provider: "claude"}}
	if c.ProviderFor("antigravity", Repository{Provider: "codex"}) != "antigravity" || c.ProviderFor("", Repository{Provider: "codex"}) != "codex" || c.ProviderFor("", Repository{}) != "claude" {
		t.Fatal("job/repo/configの優先順が異なります")
	}
	c.Provider = ""
	if c.ProviderFor("", Repository{}) != "codex" {
		t.Fatal("無指定の旧ジョブが停止します")
	}
}

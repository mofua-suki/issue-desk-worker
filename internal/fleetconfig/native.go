package fleetconfig

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"

	"issue-desk-worker/internal/gitea"
	"issue-desk-worker/internal/intake"
)

type HostAssignment struct {
	Role             string   `json:"role"`
	TestRepositories []string `json:"testRepositories"`
}

func (c Configuration) HostAssignment() HostAssignment {
	h := HostAssignment{Role: c.Assignments.HostRoles[c.Host], TestRepositories: c.Assignments.TestRepositories[c.Host]}
	if _, ok := c.Assignments.HostRoles[c.Host]; !ok && c.Host == c.assignmentSelf() {
		h.Role = c.HostRole
	}
	if _, ok := c.Assignments.TestRepositories[c.Host]; !ok && c.Host == c.assignmentSelf() {
		h.TestRepositories = c.TestRepositories
	}
	return h
}
func (c Configuration) assignmentSelf() string {
	if c.AssignmentHost != "" {
		return c.AssignmentHost
	}
	return c.Host
}
func (c Configuration) TestOnly() bool { return c.HostAssignment().Role == "test-only" }
func (c Configuration) NativeRepositories() ([]Repository, error) {
	h := c.HostAssignment()
	if !validHostRole(h.Role) {
		return nil, errors.New("host roleが不正です")
	}
	var out []Repository
	for _, name := range h.TestRepositories {
		if !gitea.ValidRepo(name) {
			return nil, errors.New("testRepositoriesが不正です")
		}
		repo, err := c.Repository(name)
		if err != nil {
			return nil, err
		}
		if repo.NativeTestHost != c.Host {
			return nil, errors.New("nativeTestHostとtestRepositoriesが一致しません")
		}
		if !slices.ContainsFunc(out, func(r Repository) bool { return r.Name == name }) {
			out = append(out, repo)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("このホストのtestRepositoriesがありません")
	}
	return out, nil
}

// DecodeApple deliberately does not require implementation-provider tools.
func DecodeApple(snapshot intake.ConfigSnapshot, host string) (Configuration, error) {
	var c Configuration
	if json.Unmarshal(snapshot.Config, &c.Fleet) != nil {
		return c, errors.New("実行設定のJSONが不正です")
	}
	c.Host = host
	c.Version = snapshot.ConfigurationVersion
	c.SettingsVersion = snapshot.SettingsVersion
	if c.Git == "" || c.GiteaURL == "" || !filepath.IsAbs(c.WorkerTokenFile) {
		return c, errors.New("Git設定が不足しています")
	}
	if c.WorkerTokenFile == c.TokenFile {
		return c, errors.New("管理トークンを使用できません")
	}
	if c.TestTimeoutSeconds <= 0 {
		c.TestTimeoutSeconds = 900
	}
	_, err := c.NativeRepositories()
	return c, err
}

// validHostRole is an empty (implementation), implementation or test-only role.
func validHostRole(role string) bool {
	return role == "" || role == "implementation" || role == "test-only"
}

package fleetconfig

import (
	"encoding/json"
	"strings"
)

func (c Configuration) Capacity() int {
	limit := c.Assignments.HostLimits[c.Host]
	if limit == 0 {
		var hosts map[string]struct {
			Max int `json:"maxImplementations"`
		}
		if json.Unmarshal(c.Assignments.Hosts, &hosts) == nil {
			limit = hosts[c.Host].Max
		}
	}
	if limit < 1 {
		return 1
	}
	if limit > 8 {
		return 8
	}
	return limit
}
func (c Configuration) RepoLimits() map[string]int {
	out := map[string]int{}
	if c.TestOnly() {
		return out
	}
	for _, r := range c.Repositories {
		allowed := false
		for repo, hosts := range c.Assignments.Implementers {
			if strings.EqualFold(repo, r.Name) {
				for _, host := range hosts {
					if host == c.Host {
						allowed = true
					}
				}
			}
		}
		if !allowed {
			continue
		}
		limit := c.Capacity()
		for repo, n := range c.Assignments.HostRepositoryLimits[c.Host] {
			if strings.EqualFold(repo, r.Name) {
				limit = n
			}
		}
		out[r.Name] = limit
	}
	return out
}

func (c Configuration) ProviderFor(job string, repo Repository) string {
	if job != "" {
		return job
	}
	if repo.Provider != "" {
		return repo.Provider
	}
	if c.Provider != "" {
		return c.Provider
	}
	return "codex"
}

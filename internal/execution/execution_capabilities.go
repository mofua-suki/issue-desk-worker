package execution

import (
	"context"
	"issue-desk-worker/internal/process"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// These are observations of the controller toolchain, not model declarations.
// Unknown facilities stay nil; a binary's presence alone does not prove a service.
func (e *Executor) executionCapabilities() map[string]any {
	env := map[string]any{"windows": runtime.GOOS == "windows", "gcc": false, "cgo": nil, "race": nil, "docker": nil, "browser": nil, "systemd": nil, "network": nil}
	if _, err := exec.LookPath("gcc"); err == nil {
		env["gcc"] = true
	}
	if goPath, err := exec.LookPath("go"); err == nil {
		out, err := process.Run(context.Background(), goPath, []string{"env", "CGO_ENABLED", "CC"}, process.Options{Timeout: 5 * time.Second, Env: map[string]string{"GOTOOLCHAIN": "local"}})
		lines := strings.Fields(out.Out)
		if err == nil && out.Code == 0 && len(lines) >= 2 {
			env["cgo"] = lines[0] == "1"
			_, ccErr := exec.LookPath(lines[1])
			env["race"] = lines[0] == "1" && ccErr == nil && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
		}
	}
	for capability, tool := range map[string]string{"docker": "docker", "browser": "chromium", "systemd": "systemctl"} {
		candidates := []string{tool}
		if capability == "browser" {
			candidates = append(candidates, "/usr/lib/chromium/chromium-headless-shell")
		}
		present := false
		for _, candidate := range candidates {
			if _, err := exec.LookPath(candidate); err == nil {
				present = true
				break
			}
		}
		if !present {
			env[capability] = false
		}
	}
	return env
}

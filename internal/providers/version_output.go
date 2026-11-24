package providers

import (
	"regexp"
	"strings"
)

var cliVersion = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z.-]+)?`)
var codexVersionLabel = regexp.MustCompile(`(?i)^\s*codex-cli\s+`)
var claudeVersionLabel = regexp.MustCompile(`(?i)^\s*(?:Claude Code\s+|\S+\s+\(Claude Code\))`)
var versionUpdate = regexp.MustCompile("(?i)\\b(?:update available|new version available|please update|run\\s+(?:`?claude\\s+update|`?codex\\s+update))\\b")
var semverCore = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var semverIdentifiers = regexp.MustCompile(`^[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*$`)
var numericIdentifier = regexp.MustCompile(`^[0-9]+$`)

func parseCLIVersion(cli, output string) string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	label := codexVersionLabel
	if cli == "claude" {
		label = claudeVersionLabel
	}
	chosen := ""
	for _, line := range lines {
		if label.MatchString(line) {
			chosen = line
			break
		}
	}
	if chosen == "" {
		for _, line := range lines {
			if strings.TrimSpace(line) != "" && !versionUpdate.MatchString(line) {
				chosen = line
				break
			}
		}
	}
	for _, location := range cliVersion.FindAllStringIndex(chosen, -1) {
		if location[0] > 0 {
			before := chosen[location[0]-1]
			if before == '.' || before >= '0' && before <= '9' {
				continue
			}
		}
		v := chosen[location[0]:location[1]]
		if validVersion(v) {
			return v
		}
		return ""
	}
	return ""
}
func validVersion(v string) bool {
	if len(v) > 100 {
		return false
	}
	main, build, hasBuild := strings.Cut(v, "+")
	if hasBuild && !semverIdentifiers.MatchString(build) {
		return false
	}
	core, pre, hasPre := strings.Cut(main, "-")
	if !semverCore.MatchString(core) {
		return false
	}
	if hasPre {
		if !semverIdentifiers.MatchString(pre) {
			return false
		}
		for _, part := range strings.Split(pre, ".") {
			if numericIdentifier.MatchString(part) && len(part) > 1 && part[0] == '0' {
				return false
			}
		}
	}
	return true
}

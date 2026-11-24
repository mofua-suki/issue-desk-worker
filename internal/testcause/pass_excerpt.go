package testcause

import "strings"

// passExcerpt retains runner result lines from the measured output. The marker
// distinguishes a bounded selection from a complete log, including when no
// individual result is available to support a reviewer's additional checks.
func passExcerpt(output string) string {
	const limit = 2800
	const missing = "\n[その他の出力は省略。必要な個別検証の根拠がなければ欠測]"
	var lines []string
	used := 0
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "ok ") && !strings.HasPrefix(trimmed, "ok\t") && !strings.HasPrefix(trimmed, "--- PASS:") && trimmed != "PASS" && !strings.HasPrefix(trimmed, "# pass ") && !strings.HasPrefix(trimmed, "ℹ pass ") {
			continue
		}
		if used+length(trimmed)+1 > limit {
			break
		}
		lines = append(lines, trimmed)
		used += length(trimmed) + 1
	}
	return strings.Join(lines, "\n") + missing
}

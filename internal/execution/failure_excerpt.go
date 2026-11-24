package execution

import (
	"sort"
	"strconv"
	"strings"
)

const excerptOmission = "\n... (省略) ...\n"

// failureExcerpt redacts before selecting diagnostics. Limits count Unicode characters.
func failureExcerpt(output string, limit int) string {
	output = redactKnown(output)
	if limit <= 0 {
		return ""
	}
	runes := []rune(output)
	if len(runes) <= limit {
		return output
	}
	lines := strings.Split(output, "\n")
	selected := make([]bool, len(lines))
	var diagnostics []int
	var failures []int
	for i, line := range lines {
		lower := strings.ToLower(line)
		trimmed := strings.TrimSpace(line)
		if strings.Contains(lower, "eperm") || strings.Contains(lower, "eacces") || strings.Contains(lower, "permission denied") || strings.Contains(line, "作業ツリー外のアクセス権限") {
			diagnostics = append(diagnostics, i)
		} else if strings.HasPrefix(trimmed, "--- FAIL") || strings.HasPrefix(trimmed, "FAIL") || strings.Contains(line, "panic:") || nodeFailureLine(trimmed) {
			failures = append(failures, i)
		}
	}
	markers := append(diagnostics, failures...)
	omission := []rune(excerptOmission)
	if len(markers) == 0 {
		if limit <= len(omission) {
			return string(runes[len(runes)-limit:])
		}
		return excerptOmission + string(runes[len(runes)-(limit-len(omission)):])
	}
	if limit <= len(omission) {
		return string([]rune(lines[markers[0]])[:min(limit, len([]rune(lines[markers[0]])))])
	}
	budget := limit
	chosen := make([]string, len(lines))
	var positions []int
	timeoutTests := make([]bool, len(lines))
	for i := 0; i+1 < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "panic: test timed out") && strings.TrimSpace(lines[i+1]) == "running tests:" {
			for j := i + 2; j < len(lines) && strings.TrimSpace(lines[j]) != "" && (lines[j][0] == ' ' || lines[j][0] == '\t'); j++ {
				timeoutTests[j] = true
			}
		}
	}
	add := func(i int, capLine bool) {
		if i < 0 || i >= len(lines) || selected[i] {
			return
		}
		position := sort.SearchInts(positions, i)
		prev, next := -1, len(lines)
		if position > 0 {
			prev = positions[position-1]
		}
		if position < len(positions) {
			next = positions[position]
		}
		// Replace the old gap with the actual separators around this line.
		separator := func(left, right int) int {
			if left < 0 && right == len(lines) {
				return 0
			}
			if right == left+1 {
				if left < 0 || right == len(lines) {
					return 0
				}
				return 1
			}
			return len(omission)
		}
		overhead := separator(prev, i) + separator(i, next) - separator(prev, next)
		line := []rune(lines[i])
		available := budget - overhead
		if available <= 0 {
			return
		}
		if capLine && available > limit/3 && limit/3 > 0 {
			available = limit / 3
		}
		if len(line) > available {
			if !capLine || timeoutTests[i] {
				return
			}
			line = line[:available]
		}
		chosen[i] = string(line)
		selected[i] = true
		positions = append(positions, 0)
		copy(positions[position+1:], positions[position:])
		positions[position] = i
		budget -= len(line) + overhead
	}
	// Reserve space for the first diagnostic before selecting timeout information.
	if len(diagnostics) > 0 {
		add(diagnostics[0], true)
	}
	// A Go timeout lists running tests immediately after its panic line.
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(strings.TrimSpace(lines[i]), "panic: test timed out") || strings.TrimSpace(lines[i+1]) != "running tests:" {
			continue
		}
		add(i, true)
		add(i+1, true)
		for j := i + 2; j < len(lines); j++ {
			line := lines[j]
			if strings.TrimSpace(line) == "" || (line[0] != ' ' && line[0] != '\t') {
				break
			}
			add(j, false)
		}
	}
	for _, i := range diagnostics {
		add(i, true)
	}
	for _, i := range markers {
		add(i, true)
	}
	for distance := 1; distance <= 12; distance++ {
		for _, i := range markers {
			add(i-distance, false)
			add(i+distance, false)
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		add(i, false)
	}
	var b strings.Builder
	previous := -1
	for i, line := range chosen {
		if !selected[i] {
			continue
		}
		if i > previous+1 {
			b.WriteString(excerptOmission)
		} else if previous >= 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
		previous = i
	}
	if previous < len(lines)-1 {
		b.WriteString(excerptOmission)
	}
	return b.String()
}

func nodeFailureLine(line string) bool {
	if strings.HasPrefix(line, "not ok ") || strings.HasPrefix(line, "✖") || strings.Contains(line, "AssertionError") {
		return true
	}
	if strings.HasPrefix(line, "ℹ fail ") {
		count, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "ℹ fail ")))
		return err == nil && count != 0
	}
	return false
}

func fixedTestMessage(prefix, detail string, limit int, hasReason bool) string {
	remaining := limit - len([]rune(prefix))
	if remaining <= 0 {
		return clip(prefix, limit)
	}
	if hasReason {
		reason, output, found := strings.Cut(detail, "\n")
		if found {
			reason = redactKnown(reason) + "\n"
			if len([]rune(reason)) >= remaining {
				return prefix + clip(reason, remaining)
			}
			return prefix + reason + failureExcerpt(output, remaining-len([]rune(reason)))
		}
	}
	return prefix + failureExcerpt(detail, remaining)
}

// Package apipath names an API call in errors without the values it carries.
package apipath

import (
	"regexp"
	"strings"
)

var numberSegment = regexp.MustCompile(`/[0-9]+(/|$)`)

// Template is path without its query and with numeric segments as {n}, so an
// error says which call failed without carrying its values.
func Template(path string) string {
	path, _, _ = strings.Cut(path, "?")
	for numberSegment.MatchString(path) {
		path = numberSegment.ReplaceAllString(path, "/{n}$1")
	}
	return path
}

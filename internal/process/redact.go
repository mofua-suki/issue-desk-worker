package process

import "regexp"

var urlCredentials = regexp.MustCompile(`(://)[^/\s]+@`)

// HideURLCredentials replaces the user information of URLs in command output
// (`scheme://user:password@host`), up to the last @ before the host.
func HideURLCredentials(s string) string {
	return urlCredentials.ReplaceAllString(s, "${1}[REDACTED]@")
}

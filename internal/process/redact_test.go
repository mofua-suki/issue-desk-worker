package process

import "testing"

func TestHideURLCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"https://u:p@host/x":         "https://[REDACTED]@host/x",
		"https://u:p@ss@host/x":      "https://[REDACTED]@host/x",
		"see https://host/x and me@": "see https://host/x and me@",
		"ssh://git@host:22/r":        "ssh://[REDACTED]@host:22/r",
	} {
		if got := HideURLCredentials(in); got != want {
			t.Errorf("%s: %s（期待: %s）", in, got, want)
		}
	}
}

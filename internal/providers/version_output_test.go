package providers

import "testing"

func TestCLIVersionLabelsAndUpdateNotices(t *testing.T) {
	for _, tc := range []struct{ cli, out, want string }{
		{"codex", "Update available 9.9.9\ncodex-cli 0.116.0", "0.116.0"},
		{"claude", "New version available 9.9.9\n2.1.289 (Claude Code)", "2.1.289"},
		{"claude", "Please update to 9.9.9\nClaude Code 2.1.289", "2.1.289"},
		{"codex", "Update available 9.9.9\n0.160.1-alpha.16.3+build.5", "0.160.1-alpha.16.3+build.5"},
		{"codex", "Run `codex update to 9.9.9", ""},
		{"claude", "Run claude update to 9.9.9", ""},
		{"codex", "codex-cli 01.2.3", ""},
		{"codex", "codex-cli 1.2.3-01", ""},
	} {
		if got := parseCLIVersion(tc.cli, tc.out); got != tc.want {
			t.Errorf("%q: %q != %q", tc.out, got, tc.want)
		}
	}
}

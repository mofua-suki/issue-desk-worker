package apipath

import "testing"

func TestTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"/api/v1/jobs/12/state":         "/api/v1/jobs/{n}/state",
		"/api/v1/requests/next?kind=x":  "/api/v1/requests/next",
		"/api/v1/commands/1794-ab/ack":  "/api/v1/commands/1794-ab/ack",
		"/repos/o/r/pulls/12/reviews/3": "/repos/o/r/pulls/{n}/reviews/{n}",
	} {
		if got := Template(in); got != want {
			t.Errorf("%s: %s（期待: %s）", in, got, want)
		}
	}
}

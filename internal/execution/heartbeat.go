package execution

import (
	"issue-desk-worker/internal/intake"
	"time"
)

func (e *Executor) HeartbeatStatus() intake.HostStatus {
	ids := e.controls.ids()
	publishing := 0
	for _, id := range ids {
		r, err := e.Store.Load(id)
		if err != nil {
			publishing++
			continue
		}
		if r != nil {
			switch r.Stage {
			case "test", "review", "publish", "merge", "deploy":
				publishing++
			}
		}
	}
	return intake.HostStatus{CLIVersions: e.cliVersions, IntakeHold: e.intakeHold(time.Now()), Publishing: publishing, Capabilities: e.Capabilities()}
}

package execution

import "issue-desk-worker/internal/fleetconfig"

// The fleet configuration lives in fleetconfig; these names keep the
// execution package's existing uses and callers unchanged.
type (
	Configuration           = fleetconfig.Configuration
	Fleet                   = fleetconfig.Fleet
	Repository              = fleetconfig.Repository
	Model                   = fleetconfig.Model
	RepositoryReleasePolicy = fleetconfig.RepositoryReleasePolicy
	ReleasePolicy           = fleetconfig.ReleasePolicy
	Setting                 = fleetconfig.Setting
	ReviewNote              = fleetconfig.ReviewNote
	HostAssignment          = fleetconfig.HostAssignment
)

var (
	DecodeConfig      = fleetconfig.Decode
	DecodeAppleConfig = fleetconfig.DecodeApple
)

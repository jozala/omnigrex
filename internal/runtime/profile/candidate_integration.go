//go:build integration

package profile

import piresources "github.com/jozala/omnigrex/agent/pi"

// NewCandidatePiV2 is only compiled into integration qualification binaries.
// It tests source-built images (including arm64); production has no override.
func NewCandidatePiV2(image string, platform Platform) (Profile, error) {
	c := piContract(image, platform)
	c.GoldenResourceContract = piresources.CandidateContract(platform.Arch)
	return New(c)
}

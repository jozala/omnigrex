package config

import (
	"errors"

	"github.com/jozala/omnigrex/internal/runtime/profile"
)

// RuntimeProfiles returns configured contracts, preferring Pi for operational
// probes when present. Repository Agent Profiles still select their runtime.
func (settings Config) RuntimeProfiles() ([]profile.Profile, error) {
	var profiles []profile.Profile
	for _, binding := range []struct {
		name, image string
		platform    profile.Platform
	}{
		{"pi-rpc", settings.PiRPCV1Image, settings.PiRPCV1Platform},
		{"opencode-acp", settings.OpenCodeACPV1Image, settings.OpenCodeACPV1Platform},
	} {
		if binding.image == "" {
			continue
		}
		value, err := profile.NewBuiltinV1(binding.name, binding.image, binding.platform)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, value)
	}
	if len(profiles) == 0 {
		return nil, errors.New("at least one exact Runtime Profile binding is required")
	}
	return profiles, nil
}

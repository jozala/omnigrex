package config

import "github.com/jozala/omnigrex/internal/runtime/profile"

// RuntimeProfiles returns the deployment's exact Pi contract.
func (settings Config) RuntimeProfiles() ([]profile.Profile, error) {
	value, err := profile.NewPiV1(settings.PiRPCV1Image, settings.PiRPCV1Platform)
	if err != nil {
		return nil, err
	}
	return []profile.Profile{value}, nil
}

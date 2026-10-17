package config

import "github.com/jozala/omnigrex/internal/runtime/profile"

// RuntimeProfiles returns the deployment's exact Pi contract.
func (settings Config) RuntimeProfiles() ([]profile.Profile, error) {
	value, err := profile.NewPiV2(settings.PiImage, settings.PiPlatform)
	if err != nil {
		return nil, err
	}
	return []profile.Profile{value}, nil
}

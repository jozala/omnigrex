package config

import (
	"errors"
	"path/filepath"
)

const DefaultProviderAuthRoot = "/var/lib/omnigrex/provider-auth"

// LoadProviderAuth is also used by the operator CLI, before the rest of the
// installation is configured. Authentication commands do not need GitHub keys.
func LoadProviderAuth(getenv func(string) string) (mode, directory string, err error) {
	mode = valueOrDefault(getenv("OMNIGREX_OPENAI_AUTH_MODE"), "api_key")
	directory = valueOrDefault(getenv("OMNIGREX_PROVIDER_AUTH_ROOT"), DefaultProviderAuthRoot)
	if mode != "api_key" && mode != "subscription" {
		return "", "", errors.New("OMNIGREX_OPENAI_AUTH_MODE must be api_key or subscription")
	}
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", "", errors.New("OMNIGREX_PROVIDER_AUTH_ROOT must be a clean absolute path")
	}
	return mode, directory, nil
}

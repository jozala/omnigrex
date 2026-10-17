//go:build integration && (!microsandbox || !cgo)

package pi_test

import "testing"

func providerSmokeEnvironment(t *testing.T, _, _ string) liveEnvironment {
	return dockerProviderSmokeEnvironment(t)
}

//go:build integration && (!microsandbox || !cgo)

package pi_test

import (
	"os"
	"testing"

	"github.com/jozala/omnigrex/internal/config"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func upgradeImages(t *testing.T) (string, string, profile.Platform, config.Config) {
	t.Helper()
	return os.Getenv("OMNIGREX_PI_UPGRADE_SOURCE_IMAGE"), os.Getenv("OMNIGREX_PI_UPGRADE_TARGET_IMAGE"), profile.Platform{OS: "linux", Arch: os.Getenv("OMNIGREX_PI_UPGRADE_ARCH")}, config.Config{ExecutionBackend: "docker"}
}
func upgradeNativeFactory(t *testing.T, _ config.Config, _, _ string) execution.Factory {
	t.Helper()
	t.Fatal("Microsandbox upgrade requires native Linux/KVM build")
	return nil
}

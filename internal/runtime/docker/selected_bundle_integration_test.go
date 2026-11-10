//go:build integration

package docker_test

import (
	"context"
	piresources "github.com/jozala/omnigrex/agent/pi"
	"github.com/jozala/omnigrex/internal/runtime/conformance"
	dockerruntime "github.com/jozala/omnigrex/internal/runtime/docker"
	"github.com/jozala/omnigrex/internal/runtime/profile"
	"os/exec"
	"testing"
	"time"
)

func TestSelectedAndRetainedPiBundleResources(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	bundle, err := piresources.SelectedBundle("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, "docker", "pull", "--platform", "linux/amd64", bundle.Image).CombinedOutput(); err != nil {
		t.Fatalf("selected baseline unavailable: %v\n%s", err, output)
	}
	r1, err := profile.NewPiV2(bundle.Image, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	image := readinessRegistryImage(t, "omnigrex/pi:1.4.0")
	r2, err := profile.NewCandidatePiV2(image, profile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	conformance.PiBundleInventories(t, ctx, r1, r2, dockerruntime.CheckRepositoryEnvironment)
}

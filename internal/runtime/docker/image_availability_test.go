package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	mobyclient "github.com/moby/moby/client"
)

type fakeImageAvailabilityAPI struct {
	image       string
	optionCount int
	err         error
}

func (api *fakeImageAvailabilityAPI) ImageInspect(_ context.Context, image string, options ...mobyclient.ImageInspectOption) (mobyclient.ImageInspectResult, error) {
	api.image = image
	api.optionCount = len(options)
	return mobyclient.ImageInspectResult{}, api.err
}

func TestExactImageAvailabilityInspectsRegistryDigestForPlatform(t *testing.T) {
	api := &fakeImageAvailabilityAPI{}
	closed := false
	availability := newExactImageAvailability(api, func() error { closed = true; return nil })
	image := "registry.example/omnigrex/opencode@sha256:" + strings.Repeat("a", 64)
	if err := availability.Available(context.Background(), image, runtimeprofile.Platform{OS: "linux", Arch: "arm64"}); err != nil {
		t.Fatalf("Available() error = %v", err)
	}
	if api.image != image || api.optionCount != 1 {
		t.Fatalf("ImageInspect() = (%q, %d options), want exact image and platform option", api.image, api.optionCount)
	}
	if err := availability.Close(); err != nil || !closed {
		t.Fatalf("Close() = %v, closed %t", err, closed)
	}
}

func TestExactImageAvailabilityRejectsMutableTagsAndLocalIDs(t *testing.T) {
	for _, image := range []string{
		"registry.example/omnigrex/opencode:latest",
		"sha256:" + strings.Repeat("a", 64),
	} {
		api := &fakeImageAvailabilityAPI{}
		err := newExactImageAvailability(api, func() error { return nil }).Available(context.Background(), image, runtimeprofile.Platform{OS: "linux", Arch: "amd64"})
		if !errors.Is(err, ErrInvalidExactImageAvailability) {
			t.Errorf("Available(%q) error = %v, want ErrInvalidExactImageAvailability", image, err)
		}
		if api.image != "" {
			t.Errorf("ImageInspect() called for non-exact image %q", image)
		}
	}
}

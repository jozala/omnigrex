package docker

import (
	"context"
	"errors"
	"fmt"

	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	mobyclient "github.com/moby/moby/client"
	"github.com/opencontainers/image-spec/specs-go/v1"
)

var ErrInvalidExactImageAvailability = errors.New("invalid exact image availability request")

type imageAvailabilityAPI interface {
	ImageInspect(context.Context, string, ...mobyclient.ImageInspectOption) (mobyclient.ImageInspectResult, error)
}

// ExactImageAvailability verifies locally available registry digests for their exact platform.
type ExactImageAvailability struct {
	api   imageAvailabilityAPI
	close func() error
}

func NewExactImageAvailability() (*ExactImageAvailability, error) {
	client, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create Docker image availability client: %w", err)
	}
	return &ExactImageAvailability{api: client, close: client.Close}, nil
}

func newExactImageAvailability(api imageAvailabilityAPI) *ExactImageAvailability {
	return &ExactImageAvailability{api: api, close: func() error { return nil }}
}

func (availability *ExactImageAvailability) Available(ctx context.Context, image string, platform runtimeprofile.Platform) error {
	if availability == nil || availability.api == nil || !runtimeprofile.IsExactRegistryImage(image) ||
		platform.OS != "linux" || platform.Arch != "amd64" && platform.Arch != "arm64" {
		return ErrInvalidExactImageAvailability
	}
	_, err := availability.api.ImageInspect(ctx, image, mobyclient.ImageInspectWithPlatform(&v1.Platform{
		OS: platform.OS, Architecture: platform.Arch,
	}))
	return err
}

func (availability *ExactImageAvailability) Close() error {
	if availability == nil || availability.close == nil {
		return nil
	}
	return availability.close()
}

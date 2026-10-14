package docker

import (
	"context"
	"errors"
	"fmt"

	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	mobyclient "github.com/moby/moby/client"
	"github.com/opencontainers/image-spec/specs-go/v1"
	"go.opentelemetry.io/otel/trace/noop"
)

var ErrInvalidExactImageAvailability = errors.New("invalid exact image availability request")

type imageAvailabilityAPI interface {
	Ping(context.Context, mobyclient.PingOptions) (mobyclient.PingResult, error)
	ClientVersion() string
	ImageInspect(context.Context, string, ...mobyclient.ImageInspectOption) (mobyclient.ImageInspectResult, error)
}

// ExactImageAvailability verifies locally available registry digests for their exact platform.
type ExactImageAvailability struct {
	api   imageAvailabilityAPI
	close func() error
}

func NewExactImageAvailability() (*ExactImageAvailability, error) {
	// This diagnostic client is also used by recurring readiness checks.
	client, err := mobyclient.New(mobyclient.FromEnv, mobyclient.WithTraceProvider(noop.NewTracerProvider()))
	if err != nil {
		return nil, fmt.Errorf("create Docker image availability client: %w", err)
	}
	return newExactImageAvailability(client, client.Close), nil
}

func newExactImageAvailability(api imageAvailabilityAPI, close func() error) *ExactImageAvailability {
	return &ExactImageAvailability{api: api, close: close}
}

func (availability *ExactImageAvailability) Available(ctx context.Context, image string, platform runtimeprofile.Platform) error {
	if availability == nil || availability.api == nil || !runtimeprofile.IsExactRegistryImage(image) ||
		platform.OS != "linux" || platform.Arch != "amd64" && platform.Arch != "arm64" {
		return ErrInvalidExactImageAvailability
	}
	if _, err := availability.api.Ping(ctx, mobyclient.PingOptions{NegotiateAPIVersion: true}); err != nil {
		return fmt.Errorf("negotiate Docker image inspection API: %w", err)
	}
	var options []mobyclient.ImageInspectOption
	// Platform selection requires API 1.49. Older APIs can still inspect the
	// exact digest; validate the returned platform rather than accepting a default.
	if compareAPIVersion(availability.api.ClientVersion(), "1.49") >= 0 {
		options = append(options, mobyclient.ImageInspectWithPlatform(&v1.Platform{
			OS: platform.OS, Architecture: platform.Arch,
		}))
	}
	inspected, err := availability.api.ImageInspect(ctx, image, options...)
	if err != nil {
		return err
	}
	if inspected.Os != platform.OS || inspected.Architecture != platform.Arch {
		return fmt.Errorf("%w: image platform does not match %s/%s", ErrInvalidExactImageAvailability, platform.OS, platform.Arch)
	}
	return nil
}

func (availability *ExactImageAvailability) Close() error {
	if availability == nil || availability.close == nil {
		return nil
	}
	return availability.close()
}

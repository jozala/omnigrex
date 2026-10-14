package microsandbox

import (
	"context"
	"github.com/jozala/omnigrex/internal/runtime/execution"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func (backend *Backend) Available(ctx context.Context, image string, platform profile.Platform) error {
	if !profile.IsExactRegistryImage(image) || platform != (profile.Platform{OS: "linux", Arch: "amd64"}) {
		return execution.ErrInvalidSpec
	}
	os, architecture, err := backend.platform.Image(ctx, image)
	if err != nil {
		return err
	}
	if os != platform.OS || architecture != platform.Arch {
		return execution.ErrBindingMismatch
	}
	return nil
}

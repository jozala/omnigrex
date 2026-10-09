//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"errors"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

const nativeReleaseWait = 5 * time.Second

// boundedNativeRelease bounds our wait even for SDK destructors that use
// Background internally. A timeout is an unknown release effect, not proof
// that native work was cancelled or that a guest stopped. Each independent
// release runs once, so a stuck exec close cannot prevent sandbox detachment.
func boundedNativeRelease(parent context.Context, wait time.Duration, releases ...func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), wait)
	defer cancel()
	results := make(chan error, len(releases))
	for _, release := range releases {
		go func() {
			err := release(ctx)
			if err != nil {
				// Never retain the SDK error, including through an Unwrap chain.
				err = errors.Join(execution.ErrUnknownEffect, errors.New("Microsandbox native release failed"), ctx.Err())
			}
			results <- err
		}()
	}
	var result error
	for range releases {
		select {
		case err := <-results:
			result = errors.Join(result, err)
		case <-ctx.Done():
			return errors.Join(result, execution.ErrUnknownEffect, ctx.Err(), errors.New("Microsandbox native release did not settle"))
		}
	}
	return result
}

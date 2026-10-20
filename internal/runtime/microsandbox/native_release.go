//go:build microsandbox && cgo

package microsandbox

import (
	"context"
	"errors"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

const nativeReleaseWait = 5 * time.Second

type nativeRelease struct {
	run       func(context.Context) error
	onTimeout func(context.Context)
}

// boundedNativeRelease bounds our wait even for SDK destructors that use
// Background internally. A timeout is an unknown release effect, not proof
// that native work was cancelled or that a guest stopped. Each independent
// release runs once, so a stuck exec close cannot prevent sandbox detachment.
func boundedNativeRelease(parent context.Context, wait time.Duration, releases ...nativeRelease) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), wait)
	defer cancel()
	type releaseResult struct {
		index int
		err   error
	}
	results := make(chan releaseResult, len(releases))
	for index, release := range releases {
		go func() {
			err := release.run(ctx)
			if err != nil {
				// Never retain the SDK error, including through an Unwrap chain.
				err = errors.Join(execution.ErrUnknownEffect, errors.New("Microsandbox native release failed"), ctx.Err())
			}
			results <- releaseResult{index: index, err: err}
		}()
	}
	var result error
	settled := make([]bool, len(releases))
	for range releases {
		select {
		case received := <-results:
			settled[received.index] = true
			result = errors.Join(result, received.err)
		case <-ctx.Done():
			// Report from the same branch that returns the unresolved wait.
			// Completion callbacks cannot consume/suppress this observation.
			for index, release := range releases {
				if !settled[index] && release.onTimeout != nil {
					release.onTimeout(ctx)
				}
			}
			return errors.Join(result, execution.ErrUnknownEffect, ctx.Err(), errors.New("Microsandbox native release did not settle"))
		}
	}
	return result
}

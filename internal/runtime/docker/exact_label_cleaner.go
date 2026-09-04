package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/containerd/errdefs"
	"github.com/jozala/omnigrex/internal/store"
	mobyclient "github.com/moby/moby/client"
)

const maximumExactLabelCleanupTimeout = 365 * 24 * time.Hour

var ErrInvalidExactLabelCleanup = errors.New("invalid exact-label Runtime Process cleanup")

type exactLabelCleanupAPI interface {
	ContainerList(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error)
	ContainerStop(context.Context, string, mobyclient.ContainerStopOptions) (mobyclient.ContainerStopResult, error)
	ContainerRemove(context.Context, string, mobyclient.ContainerRemoveOptions) (mobyclient.ContainerRemoveResult, error)
}

// ExactLabelCleanerOptions controls graceful Runtime Process termination.
type ExactLabelCleanerOptions struct {
	StopTimeout time.Duration
}

// ExactLabelCleaner removes containers carrying one complete, exact Runtime Process identity.
type ExactLabelCleaner struct {
	api                exactLabelCleanupAPI
	stopTimeoutSeconds int
	close              func() error
}

// NewExactLabelCleaner creates a cleanup client from the Docker environment.
func NewExactLabelCleaner(options ExactLabelCleanerOptions) (*ExactLabelCleaner, error) {
	if err := validateExactLabelCleanerOptions(options); err != nil {
		return nil, err
	}
	client, err := mobyclient.New(mobyclient.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create exact-label Docker cleanup client: %w", err)
	}
	return newExactLabelCleaner(client, client.Close, options), nil
}

func newExactLabelCleaner(api exactLabelCleanupAPI, close func() error, options ExactLabelCleanerOptions) *ExactLabelCleaner {
	seconds := int((options.StopTimeout + time.Second - 1) / time.Second)
	return &ExactLabelCleaner{api: api, stopTimeoutSeconds: seconds, close: close}
}

func validateExactLabelCleanerOptions(options ExactLabelCleanerOptions) error {
	if options.StopTimeout < time.Microsecond || options.StopTimeout > maximumExactLabelCleanupTimeout {
		return fmt.Errorf("%w: stop timeout must be positive and bounded", ErrInvalidExactLabelCleanup)
	}
	return nil
}

// Close releases the Docker client resources owned by the cleaner.
func (cleaner *ExactLabelCleaner) Close() error {
	return cleaner.close()
}

// EnsureAbsent gracefully stops and force-removes every container with the exact labels.
func (cleaner *ExactLabelCleaner) EnsureAbsent(ctx context.Context, labels map[string]string) error {
	if err := validateExactRuntimeLabels(labels); err != nil {
		return err
	}
	filters := make(mobyclient.Filters).Add("label",
		store.RuntimeLabelAssignmentID+"="+labels[store.RuntimeLabelAssignmentID],
		store.RuntimeLabelSessionID+"="+labels[store.RuntimeLabelSessionID],
		store.RuntimeLabelTurnID+"="+labels[store.RuntimeLabelTurnID],
		store.RuntimeLabelEpoch+"="+labels[store.RuntimeLabelEpoch],
	)
	for {
		listed, err := cleaner.api.ContainerList(ctx, mobyclient.ContainerListOptions{All: true, Filters: filters})
		if err != nil {
			return fmt.Errorf("list exact-label Runtime Processes: %w", err)
		}
		if len(listed.Items) == 0 {
			return nil
		}
		for _, candidate := range listed.Items {
			if candidate.ID == "" || !hasExactRuntimeLabels(candidate.Labels, labels) {
				return errors.New("Docker returned a container outside the exact Runtime Process label filter")
			}
		}
		for _, candidate := range listed.Items {
			timeout := cleaner.stopTimeoutSeconds
			_, err := cleaner.api.ContainerStop(ctx, candidate.ID, mobyclient.ContainerStopOptions{Timeout: &timeout})
			if errdefs.IsNotFound(err) {
				continue
			}
			if err != nil && !errdefs.IsNotModified(err) {
				return fmt.Errorf("stop exact-label Runtime Process: %w", err)
			}
			if _, err := cleaner.api.ContainerRemove(ctx, candidate.ID, mobyclient.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
				return fmt.Errorf("remove exact-label Runtime Process: %w", err)
			}
		}
	}
}

func validateExactRuntimeLabels(labels map[string]string) error {
	if len(labels) != 4 || !validRuntimeIdentity(labels[store.RuntimeLabelAssignmentID]) ||
		!validRuntimeIdentity(labels[store.RuntimeLabelSessionID]) || !validRuntimeIdentity(labels[store.RuntimeLabelTurnID]) {
		return fmt.Errorf("%w: identity labels are invalid", ErrInvalidExactLabelCleanup)
	}
	epoch, err := strconv.ParseInt(labels[store.RuntimeLabelEpoch], 10, 64)
	if err != nil || epoch <= 0 || strconv.FormatInt(epoch, 10) != labels[store.RuntimeLabelEpoch] {
		return fmt.Errorf("%w: execution epoch is invalid", ErrInvalidExactLabelCleanup)
	}
	return nil
}

func validRuntimeIdentity(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func hasExactRuntimeLabels(actual, expected map[string]string) bool {
	return actual[store.RuntimeLabelAssignmentID] == expected[store.RuntimeLabelAssignmentID] &&
		actual[store.RuntimeLabelSessionID] == expected[store.RuntimeLabelSessionID] &&
		actual[store.RuntimeLabelTurnID] == expected[store.RuntimeLabelTurnID] &&
		actual[store.RuntimeLabelEpoch] == expected[store.RuntimeLabelEpoch]
}

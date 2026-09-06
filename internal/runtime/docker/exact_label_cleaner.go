package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	mobyclient "github.com/moby/moby/client"
)

const maximumExactLabelCleanupTimeout = 365 * 24 * time.Hour

var ErrInvalidExactLabelCleanup = errors.New("invalid exact-label Runtime Process cleanup")

type exactLabelCleanupAPI interface {
	ContainerList(context.Context, mobyclient.ContainerListOptions) (mobyclient.ContainerListResult, error)
	ContainerInspect(context.Context, string, mobyclient.ContainerInspectOptions) (mobyclient.ContainerInspectResult, error)
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
		RuntimeProcessAssignmentLabel+"="+labels[RuntimeProcessAssignmentLabel],
		RuntimeProcessSessionLabel+"="+labels[RuntimeProcessSessionLabel],
		RuntimeProcessTurnLabel+"="+labels[RuntimeProcessTurnLabel],
		RuntimeProcessEpochLabel+"="+labels[RuntimeProcessEpochLabel],
	)
	if profile := labels[RuntimeProfileIdentityLabel]; profile != "" {
		filters = filters.Add("label", RuntimeProfileIdentityLabel+"="+profile)
	}
	for {
		listed, err := cleaner.api.ContainerList(ctx, mobyclient.ContainerListOptions{All: true, Filters: filters})
		if err != nil {
			return fmt.Errorf("list exact-label Runtime Processes: %w", err)
		}
		if len(listed.Items) == 0 {
			return nil
		}
		managed := make([]string, 0, len(listed.Items))
		for _, candidate := range listed.Items {
			if candidate.ID == "" || !hasExactRuntimeLabels(candidate.Labels, labels) {
				return errors.New("Docker returned a container outside the exact Runtime Process label filter")
			}
			if runtimeProcessCandidateLabels(candidate.Labels) {
				managed = append(managed, candidate.ID)
			}
		}
		if len(managed) == 0 {
			return nil
		}
		for _, containerID := range managed {
			timeout := cleaner.stopTimeoutSeconds
			_, err := cleaner.api.ContainerStop(ctx, containerID, mobyclient.ContainerStopOptions{Timeout: &timeout})
			if errdefs.IsNotFound(err) {
				continue
			}
			if err != nil && !errdefs.IsNotModified(err) {
				return fmt.Errorf("stop exact-label Runtime Process: %w", err)
			}
			if _, err := cleaner.api.ContainerRemove(ctx, containerID, mobyclient.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
				return fmt.Errorf("remove exact-label Runtime Process: %w", err)
			}
		}
	}
}

// EnsureManagedContainerAbsent removes one malformed marked container only after revalidating its exact Docker ID.
func (cleaner *ExactLabelCleaner) EnsureManagedContainerAbsent(ctx context.Context, containerID string) error {
	if strings.TrimSpace(containerID) == "" || strings.TrimSpace(containerID) != containerID {
		return fmt.Errorf("%w: container ID is invalid", ErrInvalidExactLabelCleanup)
	}
	for {
		inspected, err := cleaner.api.ContainerInspect(ctx, containerID, mobyclient.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect malformed managed Runtime Process: %w", err)
		}
		if inspected.Container.ID != containerID {
			return fmt.Errorf("%w: inspected container ID does not match", ErrInvalidExactLabelCleanup)
		}
		if inspected.Container.Config == nil || inspected.Container.Config.Labels[RuntimeProcessMarkerLabel] != RuntimeProcessMarkerValue {
			return fmt.Errorf("%w: container does not have the managed Runtime Process marker", ErrInvalidExactLabelCleanup)
		}
		if _, reason := parseRuntimeProcessIdentity(containerID, inspected.Container.Config.Labels); reason == "" {
			return fmt.Errorf("%w: container is no longer malformed", ErrInvalidExactLabelCleanup)
		}

		timeout := cleaner.stopTimeoutSeconds
		_, err = cleaner.api.ContainerStop(ctx, containerID, mobyclient.ContainerStopOptions{Timeout: &timeout})
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil && !errdefs.IsNotModified(err) {
			return fmt.Errorf("stop malformed managed Runtime Process: %w", err)
		}
		if _, err := cleaner.api.ContainerRemove(ctx, containerID, mobyclient.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("remove malformed managed Runtime Process: %w", err)
		}
	}
}

func validateExactRuntimeLabels(labels map[string]string) error {
	if len(labels) != 4 && len(labels) != 5 || !validCanonicalRuntimeUUID(labels[RuntimeProcessAssignmentLabel]) ||
		!validCanonicalRuntimeUUID(labels[RuntimeProcessSessionLabel]) || !validCanonicalRuntimeUUID(labels[RuntimeProcessTurnLabel]) {
		return fmt.Errorf("%w: identity labels are invalid", ErrInvalidExactLabelCleanup)
	}
	if len(labels) == 5 {
		if _, ok := parseRuntimeProfileIdentity(labels[RuntimeProfileIdentityLabel]); !ok {
			return fmt.Errorf("%w: Runtime Profile identity is invalid", ErrInvalidExactLabelCleanup)
		}
	}
	epoch, err := strconv.ParseInt(labels[RuntimeProcessEpochLabel], 10, 64)
	if err != nil || epoch <= 0 || strconv.FormatInt(epoch, 10) != labels[RuntimeProcessEpochLabel] {
		return fmt.Errorf("%w: execution epoch is invalid", ErrInvalidExactLabelCleanup)
	}
	return nil
}

func hasExactRuntimeLabels(actual, expected map[string]string) bool {
	exact := actual[RuntimeProcessAssignmentLabel] == expected[RuntimeProcessAssignmentLabel] &&
		actual[RuntimeProcessSessionLabel] == expected[RuntimeProcessSessionLabel] &&
		actual[RuntimeProcessTurnLabel] == expected[RuntimeProcessTurnLabel] &&
		actual[RuntimeProcessEpochLabel] == expected[RuntimeProcessEpochLabel]
	return exact && (expected[RuntimeProfileIdentityLabel] == "" || actual[RuntimeProfileIdentityLabel] == expected[RuntimeProfileIdentityLabel])
}

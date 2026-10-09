package microsandbox

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

func (backend *Backend) clean(ctx context.Context, key execution.Key, ledger execution.Ledger, remove bool) error {
	if ledger == nil {
		return execution.ErrInvalidSpec
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := backend.locked(ctx, key, func(record *operation) error {
		reservation, found, err := ledger.RevokeExecution(ctx, key)
		if err != nil {
			return err
		}
		record.Revoked = true
		if err := backend.persist(key, record); err != nil {
			return err
		}
		if record.Removed {
			return nil
		}
		if found && record.Digest == "" {
			record.Digest = reservation.Digest
		}
		if !found || !record.Created && (reservation.Phase == execution.Reserved || reservation.CreationSettled && reservation.Resource == "") {
			if found {
				if err := ledger.RecordNoExecution(ctx, key); err != nil {
					return err
				}
			}
			record.Stopped, record.Removed = true, remove
			return nil
		}
		actual, err := backend.inspect(ctx, nameFor(key))
		if err != nil {
			if errors.Is(err, execution.ErrNotFound) && reservation.CreationSettled && record.Stopped && record.ResourceID != "" {
				remaining, err := backend.keyResources(ctx, key)
				if err != nil {
					return err
				}
				for _, resource := range remaining {
					if resource.Labels[digestLabel] != record.Digest {
						return execution.ErrUnknownEffect
					}
				}
				if err := backend.stopRevokedResources(ctx, record, remaining); err != nil {
					return err
				}
				if remove {
					if !record.LaunchHistoryKnown || record.Started && !record.Flushed {
						return execution.ErrUnknownEffect
					}
					for _, resource := range remaining {
						if err := backend.platform.Remove(ctx, resource); err != nil {
							return errors.Join(execution.ErrUnknownEffect, err)
						}
					}
				}
				record.Removed = remove
				return nil
			}
			return err
		}
		if !backend.owned(actual, key, record.Digest) || record.ResourceID != "" && actual.ID != record.ResourceID || reservation.Resource != "" && reservation.Resource != resourceRef(actual) {
			return execution.ErrConflict
		}
		record.ResourceID, record.Created = actual.ID, true
		if !reservation.CreationSettled {
			if err := ledger.BindExecution(ctx, key, resourceRef(actual)); err != nil {
				return err
			}
		}
		others, err := backend.keyResources(ctx, key)
		if err != nil {
			return err
		}
		resources := []metadata{actual}
		for _, other := range others {
			if other.ID == actual.ID {
				continue
			}
			if other.Labels[digestLabel] != record.Digest {
				return execution.ErrConflict
			}
			resources = append(resources, other)
		}
		if err := backend.stopRevokedResources(ctx, record, resources); err != nil {
			return err
		}
		if remove {
			// Started includes ambiguous Start dispatch. Without required guest
			// writeback, terminal evidence remains unavailable and must be retained.
			// The pre-dispatch journal supplies positive NeverStarted for preparation.
			if !record.LaunchHistoryKnown || record.Started && !record.Flushed {
				return execution.ErrUnknownEffect
			}
			for _, resource := range resources {
				if err := backend.platform.Remove(ctx, resource); err != nil {
					return errors.Join(execution.ErrUnknownEffect, err)
				}
			}
			record.Removed = true
		}
		return nil
	})
	err = errors.Join(err, ctx.Err())
	if err == nil && remove {
		backend.inputs.forget(key)
	}
	return err
}

func (backend *Backend) keyResources(ctx context.Context, key execution.Key) ([]metadata, error) {
	var found []metadata
	cursor := ""
	seen := map[string]bool{}
	for {
		resources, next, err := backend.platform.List(ctx, cursor)
		if err != nil {
			return nil, err
		}
		for _, resource := range resources {
			if resource.Labels[namespaceLabel] != backend.options.Namespace {
				return nil, execution.ErrConflict
			}
			var owner execution.Key
			if json.Unmarshal([]byte(resource.Labels[identityLabel]), &owner) == nil && owner == key {
				found = append(found, resource)
			}
		}
		if next == "" {
			return found, nil
		}
		if seen[next] {
			return nil, execution.ErrUnknownEffect
		}
		seen[next] = true
		cursor = next
	}
}

func (backend *Backend) EnsureKeyStopped(ctx context.Context, key execution.Key) error {
	return backend.clean(ctx, key, backend.ledger, false)
}
func (backend *Backend) EnsureKeyAbsent(ctx context.Context, key execution.Key) error {
	return backend.clean(ctx, key, backend.ledger, true)
}
func (backend *Backend) EnsureAbsent(ctx context.Context, key execution.Key) error {
	return backend.EnsureKeyAbsent(ctx, key)
}

func (backend *Backend) ObserveKeyExit(ctx context.Context, key execution.Key) (observation execution.ExitObservation, err error) {
	err = backend.locked(ctx, key, func(record *operation) error {
		if record.Removed || record.Digest == "" {
			return nil
		}
		actual, err := backend.inspect(ctx, nameFor(key))
		if err != nil {
			if errors.Is(err, execution.ErrNotFound) {
				return nil
			}
			return err
		}
		if !backend.owned(actual, key, record.Digest) || record.ResourceID != "" && record.ResourceID != actual.ID {
			return execution.ErrConflict
		}
		if record.LaunchHistoryKnown && !record.Started {
			observation.NeverStarted = true
			observation.Resource = resourceRef(actual)
			return nil
		}
		if record.ProcessExited {
			observation = execution.ExitObservation{Resource: resourceRef(actual), Exited: true, ExitCode: record.ProcessExitCode, ExitCodeKnown: record.ProcessExitCode >= 0, Scope: "runtime-process", ObservedAt: record.ProcessExitedAt}
			return nil
		}
		if actual.Status == "Running" || actual.Status == "Paused" {
			observation.Running = true
		}
		if terminalEnvironmentStatus(actual.Status) {
			observation.Exited = true
			observation.Resource = resourceRef(actual)
			observation.Scope = "execution-environment"
			observation.ObservedAt = time.Now()
		}
		return nil
	})
	return observation, err
}

func (backend *Backend) List(ctx context.Context) (execution.Snapshot, error) {
	snapshot := execution.Snapshot{}
	groups := map[execution.Key][]execution.Ref{}
	cursor := ""
	seen := map[string]bool{}
	for {
		resources, next, err := backend.platform.List(ctx, cursor)
		if err != nil {
			return execution.Snapshot{}, err
		}
		for _, actual := range resources {
			if actual.ID == "" || actual.Name == "" || actual.Labels[namespaceLabel] != backend.options.Namespace {
				return execution.Snapshot{}, execution.ErrConflict
			}
			if reason := malformedResource(actual); reason != "" {
				snapshot.Malformed = append(snapshot.Malformed, execution.MalformedProcess{Resource: resourceRef(actual), Reason: reason})
				continue
			}
			if actual.Labels[diagnosticLabel] == "true" {
				expires, err := strconv.ParseInt(actual.Labels[expiryLabel], 10, 64)
				if err != nil {
					return execution.Snapshot{}, execution.ErrConflict
				}
				if expires <= time.Now().Unix() {
					if _, err := backend.confirmEnvironmentStopped(ctx, actual); err != nil {
						return execution.Snapshot{}, err
					}
					if err := backend.platform.Remove(ctx, actual); err != nil {
						return execution.Snapshot{}, err
					}
				}
				continue
			}
			var key execution.Key
			_ = json.Unmarshal([]byte(actual.Labels[identityLabel]), &key)
			groups[key] = append(groups[key], resourceRef(actual))
		}
		if next == "" {
			break
		}
		if seen[next] {
			return execution.Snapshot{}, execution.ErrUnknownEffect
		}
		seen[next] = true
		cursor = next
	}
	for key, resources := range groups {
		if len(resources) > 1 {
			snapshot.Duplicates = append(snapshot.Duplicates, execution.DuplicateProcesses{Identity: key, Resources: resources})
		} else {
			snapshot.Processes = append(snapshot.Processes, execution.ManagedProcess{Identity: key, Resource: resources[0]})
		}
	}
	return snapshot, nil
}

func (backend *Backend) EnsureOrphansAbsent(ctx context.Context, key execution.Key, resources []execution.Ref) error {
	if !validKey(key) {
		return execution.ErrInvalidSpec
	}
	for _, resource := range resources {
		actual, err := backend.resolveRef(ctx, resource)
		if err != nil {
			return err
		}
		var owner execution.Key
		if actual.Labels[namespaceLabel] != backend.options.Namespace || json.Unmarshal([]byte(actual.Labels[identityLabel]), &owner) != nil || owner != key {
			return execution.ErrConflict
		}
		if _, err := backend.confirmEnvironmentStopped(ctx, actual); err != nil {
			return errors.Join(execution.ErrUnknownEffect, err)
		}
		if err := backend.platform.Remove(ctx, actual); err != nil {
			return errors.Join(execution.ErrUnknownEffect, err)
		}
	}
	return nil
}
func (backend *Backend) EnsureMalformedAbsent(ctx context.Context, resource execution.MalformedProcess) error {
	actual, err := backend.resolveRef(ctx, resource.Resource)
	if err != nil {
		return err
	}
	if actual.Labels[namespaceLabel] != backend.options.Namespace {
		return execution.ErrConflict
	}
	if malformedResource(actual) == "" {
		return execution.ErrConflict
	}
	if _, err := backend.confirmEnvironmentStopped(ctx, actual); err != nil {
		return errors.Join(execution.ErrUnknownEffect, err)
	}
	return backend.platform.Remove(ctx, actual)
}

// Inventory and destructive cleanup use the same classification, so malformed
// diagnostic metadata cannot bypass recovery or be mistaken for a valid Turn.
func malformedResource(actual metadata) string {
	if marker, present := actual.Labels[diagnosticLabel]; present {
		if marker != "true" {
			return "invalid Microsandbox diagnostic marker"
		}
		if _, present := actual.Labels[identityLabel]; present {
			return "conflicting diagnostic and Turn ownership"
		}
		if _, present := actual.Labels[digestLabel]; present {
			return "conflicting diagnostic and Turn ownership"
		}
		expires, err := strconv.ParseInt(actual.Labels[expiryLabel], 10, 64)
		if err != nil || expires <= 0 {
			return "invalid Microsandbox diagnostic expiry"
		}
		return ""
	}
	var key execution.Key
	digest := actual.Labels[digestLabel]
	_, digestErr := hex.DecodeString(digest)
	if json.Unmarshal([]byte(actual.Labels[identityLabel]), &key) != nil || !validKey(key) || len(digest) != 64 || digestErr != nil {
		return "invalid Microsandbox execution identity"
	}
	return ""
}

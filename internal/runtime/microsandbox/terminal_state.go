package microsandbox

import (
	"context"
	"errors"
	"maps"
	"strings"
	"time"

	"github.com/jozala/omnigrex/internal/runtime/execution"
)

// SDK 0.7.6 has two terminal VM states. Neither establishes guest writeback,
// a process exit code, or OOM; Created/Starting/Draining are not stop receipts.
func terminalEnvironmentStatus(status string) bool {
	return strings.EqualFold(status, "stopped") || strings.EqualFold(status, "crashed")
}

const requiredFlushWait = 5 * time.Second
const forceStopWait = 10 * time.Second

// One shared flush budget covers the entire key, including duplicates. Reserve
// at least half the caller's remaining time for identity checks and termination.
func requiredFlushContext(ctx context.Context) (context.Context, context.CancelFunc) {
	wait := requiredFlushWait
	if deadline, bounded := ctx.Deadline(); bounded {
		wait = min(wait, max(time.Until(deadline)/2, 0))
	}
	return context.WithTimeout(ctx, wait)
}

// The caller holds the dispatch journal lock, has durably revoked the key, and
// has validated every resource's ownership/digest against that journal.
func (backend *Backend) stopRevokedResources(ctx context.Context, record *operation, resources []metadata) error {
	if !record.Revoked {
		return execution.ErrConflict
	}
	flushed := record.Flushed
	if len(resources) > 0 {
		flushed = true
	}
	flushCtx, cancelFlush := requiredFlushContext(ctx)
	for _, resource := range resources {
		if resource.Status == "Running" || resource.Status == "Paused" {
			if flushCtx.Err() != nil {
				flushed = false
				continue
			}
			if err := backend.platform.Pause(flushCtx, resource); err != nil {
				flushed = false
			}
		} else if !terminalEnvironmentStatus(resource.Status) || !record.Flushed {
			flushed = false
		}
	}
	cancelFlush()
	// Keep positive writeback evidence even if subsequent termination fails.
	// A revoked key cannot resume these writers through the adapter.
	record.Flushed = flushed
	flushPersistErr := backend.persist(record.Key, record)
	// Cancellation can revoke the caller's fence, but must not leave its exact
	// VM executing. Only termination gets this independent finite budget;
	// Store bindings, removal, and successful receipts retain the caller fence.
	stopCtx, cancelStop := context.WithTimeout(context.WithoutCancel(ctx), forceStopWait)
	defer cancelStop()
	var stopErr error
	for index, resource := range resources {
		stopped, err := backend.confirmEnvironmentStopped(stopCtx, resource)
		if err != nil {
			stopErr = errors.Join(stopErr, err)
			continue
		}
		resources[index] = stopped
	}
	record.Stopped = stopErr == nil
	return errors.Join(ctx.Err(), stopErr, flushPersistErr, backend.persist(record.Key, record))
}

// A lifecycle request can lose its reply or race a same-name replacement.
// Revalidate exact identity and ownership before dispatch and after termination.
// Stop covers the VM, including background processes and nested containers.
func (backend *Backend) confirmEnvironmentStopped(ctx context.Context, expected metadata) (metadata, error) {
	ctx, cancel := context.WithTimeout(ctx, forceStopWait)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return metadata{}, errors.Join(execution.ErrUnknownEffect, err)
	}
	actual, err := backend.resolveRef(ctx, resourceRef(expected))
	if err != nil {
		return metadata{}, err
	}
	if !maps.Equal(actual.Labels, expected.Labels) {
		return metadata{}, execution.ErrConflict
	}
	var stopErr error
	if !terminalEnvironmentStatus(actual.Status) {
		stopErr = backend.platform.Stop(ctx, actual)
	}
	stopped, err := backend.resolveRef(ctx, resourceRef(actual))
	if err != nil || !maps.Equal(stopped.Labels, actual.Labels) || !terminalEnvironmentStatus(stopped.Status) {
		return metadata{}, errors.Join(execution.ErrUnknownEffect, stopErr, err)
	}
	// A confirmed terminal observation settles even a lost Kill acknowledgement.
	return stopped, nil
}

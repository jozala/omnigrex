---
status: accepted
date: 2026-10-18
decision-makers: [Mariusz]
---
# Cache immutable registry documents for offline image verification

## Context
Microsandbox image verification repeatedly contacts GHCR even when the image is already provisioned locally.
The SDK reserializes cached manifests, so verification must fetch original manifest bytes to establish digest equivalence.
Index-pinned references also require the original index to prove linkage to the selected platform manifest.
Repeated readiness checks therefore create unnecessary registry traffic and availability coupling.

## Decision
Persist exact digest-addressed registry manifests and indexes in a versioned content-addressed cache under the configured Microsandbox home.
Validate cached bytes against their requested SHA-256 on every read and keep all existing SDK metadata, configuration, platform, and index-linkage checks authoritative.
Populate missing documents through the existing bounded exact-digest HTTPS fetcher with per-document cross-process locks, atomic durable publication, and shared retry cooldown.
Reuse documents across image references and Runtime Profiles by digest without expiry or background refresh.

## Rationale
Digest-addressed documents are immutable, so content validation replaces TTL and revalidation.
A persistent control-plane-owned cache survives restarts and upgrades, unlike SDK-managed files and dispatch state.
Per-document locks and atomic rename prevent partial or duplicated population across concurrent processes.
Shared cooldown avoids amplifying outages while warm hits stay usable during registry unavailability.
Rejecting mutable references and write-failure success preserves the existing verification boundary.

## Consequences
Warm readiness checks make zero registry requests for cached documents, including after orchestrator restart.
Verification still fails when SDK metadata is corrupt or the image is not provisioned, and flat-root materialization may still need registry access.
Deleting the cache is safe but requires registry access to rebuild evidence.
Cached documents are reproducible cache data, not required Session backup data.

## Links
- [Work Item: cache immutable registry documents](https://github.com/jozala/omnigrex/issues/97).
- [Pin Participant images and use working-revision tools](0009-pin-participant-images-and-use-working-revision-tools.md).
- [Use the Microsandbox Go SDK and POSIX-faithful local bindings](0013-use-the-microsandbox-go-sdk-and-posix-bindings.md).
- [Establish a clean supported release baseline](0024-establish-a-clean-supported-release-baseline.md).

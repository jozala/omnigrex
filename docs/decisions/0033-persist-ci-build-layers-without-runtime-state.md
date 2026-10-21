---
status: accepted
date: 2026-10-21
decision-makers: [Codex implementation agent]
---
# Persist CI build layers without runtime state

## Context

GitHub-hosted CI starts with a fresh Docker builder on each run, so source image and qualification image builds repeatedly download dependencies and compile tools.
The same jobs must continue testing their actual loaded image tags, and release smoke must still use the exact published digests on both execution backends.
Fork pull requests run untrusted source and cannot require write-capable registry or workflow permissions.

## Decision

Use `actions/setup-go` for the check job's host Go module and build caches, and require uncached test results for its test commands.
Restore and save a job-scoped local BuildKit layer cache through GitHub Actions cache storage for the check, Microsandbox, publication, and development-image smoke jobs.
Select a digest-pinned, multi-platform BuildKit container builder in those jobs.
Each CI image build imports and exports that cache with a tag derived from its Dockerfile and target, then loads the resulting image into the Docker daemon for existing consumers.
Use Buildx's local cache `reset=true` export to remove blobs that no cache tag references while retaining the other targets' manifests.
The check job serves source-built Pi and development images from a disposable runner-local registry on the builder's private Docker network.
Dual BuildKit outputs load those images under their existing Docker tags and publish the same build result to that registry so subsequent `FROM` stages can resolve it.
The Microsandbox gate compiles the patched guest-agent test binary in a cached layer and runs its workload tests in a separate target with a unique invocation nonce.
Keep ordinary local `docker build` behavior when the CI cache directory is unset.
Cache entries contain only rebuildable build layers, never deployment volumes, Agent Session state, or credentials.

## Rationale

Job and platform scoping prevents unrelated builds from replacing each other's cache manifests while allowing reuse across runs.
GitHub's pull-request cache restrictions allow forks to restore eligible base-branch caches without granting new permissions or allowing their entries to replace base-branch entries; saving follows the trigger's secure default.
BuildKit cache mounts do not participate in external layer-cache export, so the Microsandbox and Pi qualification Go compilation stages retain their build cache in ordinary layers instead of relying on mounts that start empty on each runner.
Registry cache export would require write access during pull-request builds and create a second publication artifact lifecycle.
The runner-local registry exists only during the check job and holds source images for its chained builds, without credentials or access to GHCR.
Buildx 0.35 or newer is needed for pruning unreferenced blobs during local cache export.

## Consequences

First runs and invalidated layers still perform full builds, and cache transfer itself has a cost.
The GitHub Actions cache is disposable; correctness and the release's exact-digest gates remain independent of a hit.
The existing cold, offline, and module-fallback qualification continues to run after image construction.
The guest-agent test target keeps executable test coverage when its compilation layer is restored.

## Links

- [Image-local module source](0026-use-an-image-local-go-module-source.md).
- [Release gate policy](0018-gate-pi-publication-on-both-backend-deployments.md).

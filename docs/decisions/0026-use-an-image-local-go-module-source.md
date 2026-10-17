---
status: accepted
date: 2026-10-16
decision-makers: [Mariusz]
---
# Use an image-local Go module source for the development environment

## Context

Omnigrex needs a Pi golden-derived development image that supports its own Docker-backed tests on Microsandbox.
New Agent Participants otherwise start with cold Go module caches, while the existing persistent tool-data attachments already retain module and build caches across Agent Turns.
The requested image optimization should work through the existing repository environment and working-revision tooling contracts.

## Decision

Package downloaded project modules and the pinned Go development tool's dependencies as a read-only file-based Go module proxy inside the development image.
Use repository mise environment configuration to prepend that local source when the directory exists, retaining the configured upstream proxy as fallback.
Let Go populate each Participant's existing writable module cache on demand, without adding cache-copy behavior to the Omnigrex runtime or changing the golden bootstrap.

Publish the development image against the same release's exact golden digest.
Local development builds use a same-checkout, same-platform source-built golden tag so they cannot silently select an incompatible pre-baseline bootstrap before publication.
Produce the promotable repository environment document only after exact-digest cache and Microsandbox qualification succeeds.
Promotion remains a repository change; retained Participants keep their immutable image bindings.

## Rationale

A startup copy mechanism adds platform behavior and copies modules that a particular working revision may never need.
Go's native file proxy supports lazy local reads, normal checksum validation, and fallback for dependencies absent from the image.
A writable cache baked directly into the image would be hidden by runtime attachments or confused with disposable tool-installation data.

## Consequences

The image is larger, and selected archives are still copied and extracted into each Participant's writable cache.
The working revision's module files remain authoritative.
Go toolchain installation, compiled build artifacts, checksum lookups, and Docker build-layer caches are separate concerns; this optimization does not guarantee offline startup.

## Links

- [Participant image and tooling policy](0009-pin-participant-images-and-use-working-revision-tools.md).
- [Repository environment qualification](0012-qualify-repository-environments-without-rebinding-sessions.md).
- [Development image delivery plan](../plans/2026-10-16-development-image.md).

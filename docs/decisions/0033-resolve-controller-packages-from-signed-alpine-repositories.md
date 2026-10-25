---
status: accepted
date: 2026-10-20
decision-makers: [Mariusz Józala]
---
# Resolve controller packages from signed distribution repositories

## Context
The Docker controller image installed an exact per-architecture APK filename closure with repository-held SHA-256 hashes.
Ordinary Alpine stable repositories replace package revisions without retaining every old artifact, so the lock froze dependency selection without preserving its inputs.
On 2026-10-20 a direct check still returned HTTP 404 for `libexpat-2.8.5-r0` while both repository listings offered `libexpat-2.9.0-r0`.
Refreshing one pin would not fix the recurring retention failure, and removing checksum verification alone would still request the missing filename.
On 2026-10-25, Mariusz extended this policy to Microsandbox's Ubuntu-based controller after its pinned Git package also returned HTTP 404.

## Decision
Install the controller tools from the selected Alpine stable release with normal trusted signing-key verification at build time.
Keep the digest-pinned Alpine base image and the default stable repositories, and let APK resolve the available dependency closure from the top-level packages declared in the `Dockerfile`.
Generate a sorted installed-package inventory with `apk info -v | sort` at `/usr/share/omnigrex/apk-packages.txt` after installation.
For Microsandbox, keep the digest-pinned base and install Git directly with normal APT verification, without a separate package lock, inventory, or download stage.
Keep exact image digests as the unit of deployment and recovery.

## Rationale
Signature verification preserves supply-chain authentication without pretending that an artifact allowlist can outlive repository pruning.
A top-level set preserves controller capabilities while accepting that later builds of the same source may resolve different signed revisions.
An image-bound inventory records what was actually installed without attempting to embed the final image digest inside itself.
Exact filename downloads, repository-held hashes, architecture filename mapping, offline installation, and fixed-count checks were removed because each reintroduced the same retention brittleness.

## Consequences
Later builds of unchanged sources may produce different image digests with no dependency-lock diff.
Already deployed digest-pinned images are unchanged, and released images must be retained because rebuilding a source commit is not recovery.

## Links
- [Work Item: make Docker controller builds resilient to removed Alpine packages](https://github.com/jozala/omnigrex/issues/104).
- [Microsandbox package download failure](https://github.com/jozala/omnigrex/actions/runs/40185861062/job/121146375568?pr=118).
- [Docker controller package policy](../contributors/development.md#docker-controller-package-policy).
- [Pin Participant images and use working-revision tools](0009-pin-participant-images-and-use-working-revision-tools.md).
- [Establish a clean supported release baseline](0024-establish-a-clean-supported-release-baseline.md).

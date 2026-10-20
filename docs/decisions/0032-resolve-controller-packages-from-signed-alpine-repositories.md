---
status: accepted
date: 2026-10-20
decision-makers: [Mariusz Józala]
---
# Resolve Docker controller packages from signed Alpine repositories

## Context
The Docker controller image installed an exact per-architecture APK filename closure with repository-held SHA-256 hashes.
Ordinary Alpine stable repositories replace package revisions without retaining every old artifact, so the lock froze dependency selection without preserving its inputs.
On 2026-10-20 a direct check still returned HTTP 404 for `libexpat-2.8.5-r0` while both repository listings offered `libexpat-2.9.0-r0`.
Refreshing one pin would not fix the recurring retention failure, and removing checksum verification alone would still request the missing filename.

## Decision
Install the controller tools from the selected Alpine stable release with normal trusted signing-key verification at build time.
Keep the digest-pinned `alpine:3.24` base image and the default stable repositories.
Require only the top-level set `bash`, `ca-certificates`, `curl`, `git`, `jq`, `gcc`, `musl-dev`, `openssh-client-default`, and `ripgrep`, and let APK resolve the available dependency closure.
Generate a sorted installed-package inventory with `apk info -v | sort` at `/usr/share/omnigrex/apk-packages.txt` after installation.
Keep exact image digests as the unit of deployment and recovery.

## Rationale
Signature verification preserves supply-chain authentication without pretending that an artifact allowlist can outlive repository pruning.
A top-level set preserves controller capabilities while accepting that later builds of the same source may resolve different signed revisions.
An image-bound inventory records what was actually installed without attempting to embed the final image digest inside itself.
Exact filename downloads, repository-held hashes, architecture filename mapping, offline installation, and fixed-count checks were removed because each reintroduced the same retention brittleness.

## Consequences
Later builds of unchanged sources may produce different image digests with no dependency-lock diff.
Already deployed digest-pinned images are unchanged, and released images must be retained because rebuilding a source commit is not recovery.
Refresh packages intentionally with `docker buildx build --no-cache-filter runtime ...` or an equivalent verified command.
Docker build-layer caching and APK `--no-cache` remain distinct mechanisms.
The analogous Ubuntu artifact-retention concern in `Dockerfile.microsandbox` is separate follow-up without distribution or compatibility-check changes.

## Links
- [Work Item: make Docker controller builds resilient to removed Alpine packages](https://github.com/jozala/omnigrex/issues/104).
- [Pin Participant images and use working-revision tools](0009-pin-participant-images-and-use-working-revision-tools.md).
- [Establish a clean supported release baseline](0024-establish-a-clean-supported-release-baseline.md).

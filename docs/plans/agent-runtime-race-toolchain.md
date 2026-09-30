# Runtime Process Go Race Toolchain Fix

## Goal

Allow Agent Turns to compile and execute race-enabled Go tests using the repository's pinned Go toolchain under the existing Runtime Process hardening.
The OpenCode version remains `1.18.29`; the packaging change creates a new image digest.

## Diagnosis

The current images include `libgcc` but omit the GCC compiler and musl development headers.
Go automatically disables cgo when it cannot find a C compiler, so `go test -race` fails before compilation.
Forcing `CGO_ENABLED=1` then fails because `gcc` is unavailable.
Installing `gcc`, `musl-dev`, and their dependencies in a disposable amd64 image enabled cgo automatically and allowed the standard library's race tests to pass as UID `10001`.

## Image Changes

1. Extend `agent/opencode/apk-packages.sha256` with exact compiler, development-header, and dependency artifacts for both `amd64` and `arm64`.
2. Retain checksum verification and installation without repository access during the image build.
3. Keep the manifest shared between the orchestrator and Runtime Process so repository tools can be provisioned and executed with matching cgo dependencies.
4. Update the expected package count in both Dockerfiles.
5. Let Go detect the compiler instead of forcing `CGO_ENABLED` in the image environment.

Refresh the exact `ca-certificates` and `libexpat` pins for both architectures because Alpine has withdrawn their previously pinned artifacts and clean builds need the available replacements.

## Regression Gate

Add `TestOpenCodeImageRunsGoRaceTests` to `internal/runtime/docker` with the integration build tag.
Read the repository's pinned Go version from `go.mod` and install it through mise into a disposable assignment volume.
Use the candidate image's architecture and the built-in Runtime Profile's identity, mount targets, tmpfs limits, and process limit.
Execute the fixtures with a read-only root filesystem, capabilities dropped, `no-new-privileges`, networking disabled, and disk-backed `TMPDIR`, `GOTMPDIR`, `GOCACHE`, and `GOPATH` paths.
Require cgo to be enabled without an environment override and require a synchronized concurrent test to pass with `-race`.
Require an intentionally unsynchronized concurrent test to exit unsuccessfully and report `WARNING: DATA RACE`.
Bound tool installation and cold compilation separately from ordinary 30-second Docker probes, and clean up every test-owned container and volume.
Keep the combined phases below the outer package timeout so cancellation still permits test-owned container and volume cleanup.
The old image must fail this gate, while the corrected image must pass it on both supported architectures.
The existing local and CI Docker-backed test commands already include this package.

## Verification

```sh
go test -race -tags=integration -timeout 30m ./internal/runtime/docker -run '^TestOpenCodeImageRunsGoRaceTests$' -count=1 -v
mise run check
mise run test-integration
```

Build both images with the shared manifest and build and exercise the Runtime Process image on `linux/amd64` and `linux/arm64`.
Run destructive Compose checks only where their stable local resources can be removed.

## Documentation And Rollout

Document the build-tool contract and focused validation command in `agent/opencode/README.md`.
Qualify the actual deployed source digest against the corrected candidate digest using the [Runtime Profile image-change procedure](../operator-guide.md#runtime-profile-image-change), even when both contain the same OpenCode version.
Existing Agent Sessions remain pinned to the original image digest.
An existing Workflow can reuse those sessions after a new `omnigrex:run`, so adopting the corrected tools for that Workflow requires a separate supported session-lifecycle decision.
The test-fake race reported by PR #16 remains a distinct Change Proposal issue; this fix gives future Agent Turns the tooling needed to reproduce it.

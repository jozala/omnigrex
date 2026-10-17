# Retire superseded runtime experiments

The cleanup follows [decision 0024](../decisions/0024-establish-a-clean-supported-release-baseline.md) and the operator's request to retire historical executable scaffolding while preserving useful coverage.

## Coverage ownership

| Retired prototype behavior | Maintained coverage |
| --- | --- |
| Raw Pi RPC framing, concurrent response correlation, admission, cancellation, and provider failure | `internal/runtime/pi` unit and controlled-provider integration tests |
| Authenticated MCP, exact model/thinking selection, instruction/resource exclusion, and credential handling | Production Pi configuration and Role authority qualification |
| Session identity, fresh-environment continuation, and provider-free replay | Production durable Session, protected-state recovery, and source-to-candidate upgrade gates |
| Mid-prompt workspace capture | Production Docker and Microsandbox workspace/capture tests and interrupted-publication recovery |
| SDK streams, separate stderr, detach, cancellation, pause/resume, and whole-environment stop | Production Microsandbox conformance, storage/capture, transport-release, and lifecycle tests |
| Private guest Docker, Compose, and Testcontainers | Production Microsandbox nested-container gate, with fixtures moved to `internal/runtime/microsandbox/testdata/nested` |
| Real API-key provider smoke | `internal/runtime/pi/live_integration_test.go` and `scripts/test-pi-live.sh`, using the production golden image, renderer, runner, and backend |

## Changes

- Remove both experiment Dockerfiles, standalone SDK/CLI harnesses, duplicate RPC clients, prototype MCP extensions, and obsolete feasibility commands.
- Preserve historical findings under `docs/experiments/` with source-history links.
- Move the nested-container fixtures into the owning production backend package and update its qualification image.
- Keep `mise run test-pi-live` as an opt-in production-path API-key smoke on both backends.
- Remove the unqualified subscription snapshot rather than maintaining a separate unsupported authentication path.
- Remove the obsolete Alpine repository-image example and point documentation to the maintained Debian Pi example.

## Verification

The production live gate requires an explicitly supplied credential file and is not implied by controlled-provider or historical experiment results.

Passed on macOS/arm64 with Docker Desktop Linux/arm64:

- `mise run check`.
- `mise run test-integration`, including the complete Store suite and migration package.
- `mise run test-pi-integration`.
- `mise run test-pi-upgrade`, including continuation, exact provider-free replay, isolated restore, and unchanged source state.
- `mise exec -- go test -race ./internal/deployment -run '^TestPiLiveSmoke' -count=1`, verifying both backend command paths deliver credentials only through stdin into tmpfs and preserve the supplied file.
- Integration-tagged `TestProviderSmokeConfigurationAndArchiveAudit` and `TestProviderSmokeMCPRequiresAuthenticationAndRejectsBusinessCalls`.
- The new Docker harness with `OMNIGREX_PI_LIVE_TEST_PATTERN='^TestPiProviderSmokeBootstrap$'` and fixture keys, exercising real golden-resource qualification, owned-runner startup, exact model/thinking selection, authenticated MCP discovery, durable Session creation, stopped-state export, and credential auditing for both providers without a provider request.
- Shell syntax and `git diff --check`.

Both `qualification` and `pi-qualification` targets of `Dockerfile.microsandbox` built for Linux/amd64.
The qualification image contains all four relocated nested-container fixture files, whose SHA-256 checksums match their original contents.
The native Pi image passed its non-KVM provider-smoke configuration, authentication, and archive-audit helper tests under emulation.

Actual Microsandbox execution was initially blocked because the approved Linux/KVM host was unreachable.
Access was restored during the [Participant compatibility cleanup](2026-10-17-participant-compatibility-cleanup.md#verification), and the full native backend gate plus provider-smoke bootstrap passed on that follow-up source.
This includes the relocated nested Docker/Compose/Testcontainers fixtures and production-path bootstrap/state audits for both providers on real KVM.
Real provider prompting was not run for this follow-up; historical live-provider results are not reused as evidence for the migrated gate.

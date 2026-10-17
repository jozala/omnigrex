# Clean runtime release baseline

The operator selected a one-time breaking reset and retained future Pi-to-Pi upgrade support.
[Decision 0024](../decisions/0024-establish-a-clean-supported-release-baseline.md) owns the policy.

## Required outcomes

1. One initial schema contains current Workflow, fencing, Session, capture, retention, and qualification structures without retired protocol fields or tables.
2. Only `pi-rpc/v2` is supported, with neutral guest paths and version-neutral image/platform settings.
3. Startup rejects incompatible existing migration history before applying new schema changes.
4. Deployment smoke has no historical cutover mode and retains independent Pi recovery coverage.
5. Exact source-to-candidate qualification, protected-state recovery, and replay continue on Docker and Microsandbox from the new baseline.
6. Current configuration examples and repository Profiles match the implementation.

## Implementation sequence

- Establish schema-invariant and incompatible-history regression tests, then replace the accumulated schema with a canonical baseline.
- Replace migration-only fixtures with current-schema behavior and migration-engine tests.
- Advance the runtime contract and update guest paths, configuration, and both-backend fixtures together.
- Remove unused preparation compatibility inputs and simplify execution-readiness probes.
- Separate current Pi recovery from historical reset tooling and rebase upgrade fixtures on the clean contract.
- Update current documentation and verify focused tests, `mise run check`, and Docker/Microsandbox gates.

## Verification boundaries

Docker is available locally on Linux/arm64 through Docker Desktop.
Native Microsandbox requires an approved Linux/amd64 Docker host with usable KVM; report unavailable native execution explicitly rather than substituting another backend or skipping failures.
No production deployment or storage disposal is authorized by this implementation task.

## Delivered implementation

- Replaced the 36 accumulated migrations with one current-schema baseline, retaining current Workflow, ownership, fencing, Session, capture, retention, and backend-scoped qualification structures.
- Removed retired protocol columns, historical compatibility storage, migration-only recovery behavior, unused preparation inputs, and former Assignment API aliases.
- Made backend ownership an atomic immutable first claim and required complete Participant and Session Runtime Profile bindings.
- Advanced runtime selection to `pi-rpc/v2`, with `OMNIGREX_PI_IMAGE`, `OMNIGREX_PI_PLATFORM`, and neutral guest tool paths throughout configuration, images, execution, and fixtures.
- Removed Docker execution-readiness fallback without an exact selected Runtime Profile.
- Replaced historical cutover execution tooling with independent stopped-execution recovery smoke, enabled by default for both deployment bundles.
- Preserved directional clean-baseline Pi image upgrade qualification, isolated state copies, exact protected source bindings, provider-free History Replay, and compatible-host recovery.
- Added regressions for reserved-Creating Session eligibility and full Session/Participant binding agreement before sealing Workspace Capture evidence.
- Updated current operation and dogfooding documentation and archived prior-release live evidence.

## Verification evidence

Verification on 2026-10-16 used the working changes based on `8168c36`.
All backend scenarios used isolated fixtures and controlled providers.

| Environment | Command or check | Result |
| --- | --- | --- |
| macOS/arm64 with Docker Desktop Linux/arm64 | `mise run check` | Passed after the final migration review fix. |
| macOS/arm64 with Docker Desktop Linux/arm64 | `mise run test-integration` | Passed. |
| macOS/arm64 with Docker Desktop Linux/arm64 | `mise run test-docker-integration` | Passed, including independent coordinated recovery smoke. |
| macOS/arm64 with Docker Desktop Linux/arm64 | `mise exec -- go test -race -tags=integration ./internal/store/migrations -run '^TestForward' -count=1` | Passed after the final migration review fix. |
| macOS/arm64 with Docker Desktop Linux/arm64 | `mise exec -- go test -p 1 -race -tags=integration -timeout 10m ./internal/store ./internal/store/migrations -run 'Test.*(Baseline\|Migration\|BackendBinding\|Capture)' -count=1` | Passed after the final migration review fix. |
| Linux/amd64 with Docker and KVM | `mise run check` | Passed, including a rerun after the final migration review fix. |
| Linux/amd64 with Docker and KVM | `mise run test-integration` | Passed after the final migration review fix, including `internal/store/migrations`. |
| Linux/amd64 with Docker and KVM | `mise run test-docker-integration` | Passed. |
| Linux/amd64 with Docker and KVM | `OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-microsandbox` | Passed. |
| Working changes | `git diff --check` | Passed. |

The Linux backend aggregate completed with exit status 0.
Its Microsandbox evidence includes non-root execution, private nested Docker, guest cgroup budgets, Compose, Testcontainers/Ryuk, and whole-VM termination with a running nested container.
Both backends passed controlled Developer-to-Reviewer Workflows, slow repository bootstrap, protected-state recovery, failure recovery, source-to-candidate continuation and replay, and image-only deployment with coordinated ledger/state/capture recovery.
Microsandbox recovery also preserved the native operation journal.

The backend aggregate preceded the final review fix, which added injectable migration sources to the existing runner and direct forward-upgrade regression tests.
The final Linux shared integration rerun passed with that fix and explicitly included `internal/store/migrations`.
That rerun completed in 835.15 seconds, with Store passing in 797.268 seconds and the migration package in 6.786 seconds.
The backend execution and recovery implementations were unchanged by that fix.

The deployment diagnostics intentionally use disposable GitHub App credentials, so their expected GitHub-installation failures do not establish live GitHub validation.
These results establish source qualification; publication, exact published-digest smoke, operator deployment, and live Work Item validation remain tracked by the [dogfooding record](../dogfooding.md#current-baseline-validation).

## Review evidence

The first detailed review round covered schema/migration safety, runtime/deployment/recovery, and requirements/upgrade preservation.
It identified one important coverage gap: direct verification of future forward-migration prefix preservation and interrupted pending SQL.
The correction exercises the production migration runner with synthetic migration sets, proving committed-prefix preservation, pending-suffix rollback and retry, cancellation during SQL, lock release, and rejection of non-prefix history before application mutation.
Local integration and CI commands now include that package.

A fresh review round inspected the corrected working changes, including untracked files, for schema/ownership/fencing and runtime/upgrade/deployment behavior.
Both reviews reported no important findings.

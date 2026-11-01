# Choose the right verification gate

[Contributor guide](README.md) · [Runtime qualification](runtime-qualification.md)

## Normal local work

```sh
mise run check
```

This runs formatting checks, `go vet`, dead-code analysis, race-enabled unit tests, Compose validation, and repository-local documentation links.
Run the narrowest relevant tests during implementation, then this gate for code changes.
Use `mise run docs-check` for a documentation-only link/anchor pass.
See [mise.toml](../../mise.toml) for the exact executable commands.

## Docker-backed integration

```sh
mise run test-integration
```

This builds the Pi image and checks doctor, Docker runtime, PostgreSQL/store/migrations, and deployment behavior with Docker-backed fixtures.
Use Docker Engine or Docker Desktop with enough capacity and network access for fixture images.

The executable documentation examples can be checked independently:

```sh
mise exec -- go test ./internal/deployment -run TestPiOperatorRestoreRequiresConfirmedNativeAbsence -count=1
mise exec -- go test -tags=integration ./internal/deployment -run TestPiOperatorExamplesPreserveComposeSelection -count=1
```

The restore test uses stub commands and temporary data.
The Compose example test renders the documented selections without deploying or changing protected state.
The Docker-backed gate exercises the controller image check described in [source builds](development.md#docker-controller-package-policy).

## Investigate test duration

The PR `check` job captures the host-side unit, general Docker integration, and Pi protocol test commands with `go test -json -count=1`.
It preserves readable test progress and test failures, publishes a job summary, and uploads the `check-test-timings` artifact for seven days, including available evidence after failure.
An interrupted or unstarted command is reported as incomplete, not as zero time.
Missing, null, or negative terminal-event durations also make the capture incomplete; their costs are `null` in JSON and unavailable in Markdown, while an explicit zero remains a valid measurement.
An unfinished command's wall time is likewise `null`, and a measured wall time remains available even when test-event data is incomplete.
Observed packages and named tests must have terminal events; output alone does not establish a complete test execution.

To measure the store suite locally after preparing the [Docker integration prerequisites](#docker-backed-integration):

```sh
mise exec -- go build -o bin/test-timings ./scripts/test-timings
mise exec -- bin/test-timings run coverage/test-timings integration -- sh scripts/go-test.sh -p 1 -race -tags=integration -count=1 -json -timeout 30m ./internal/store
mise exec -- bin/test-timings report coverage/test-timings integration
```

Use a fresh output directory for each comparison, or remove only that comparison's old timing files first.
Each capture writes `<label>.events.jsonl` and `<label>.run.json`; reporting writes `summary.md` and `summary.json`.
The JSON summary includes all completed test results and fixture samples, while the Markdown tables show the slowest twenty entries per category.
The runner expects a JSON-producing Go test command and returns its failing exit code; malformed capture output also fails an otherwise successful command.

Ordinary store integration fixtures lazily share one PostgreSQL process per test-binary invocation, but each fixture gets a distinct empty database cloned from `template0`.
The normal migration path still runs for every store initialization; no migrated application template, shared tables, or rollback-only isolation replaces fresh databases.
Test cleanup drops only its database, including any leaked connections, and `TestMain` removes the owned shared container after all tests finish.
Dedicated startup/password and concurrent-migration tests, the fresh/existing migration probes, and the separate migration-engine package retain fresh PostgreSQL processes.
The shared server retains filesystem hardening and normal durability settings; bounded WAL/checkpoint settings keep repeated fixture creation within its disposable tmpfs budget.
This changes fixture allocation, not the suite's application-test parallelism.

Store integration fixtures log PostgreSQL container startup, port discovery, readiness polling, removal, isolated database creation/drop, and each writable `store.Open` call.
Reports count container-start attempts separately from per-database creation attempts so process reuse is visible.
Internal fixture phases from nested helper subprocesses are not merged into the parent command's phase counts, although their runtime remains included in the owning test's duration.
Fixture identifiers are process-local counters, and timing records contain no connection strings, passwords, SQL parameters, or error messages.
Raw Go test output still has the same sensitivity as ordinary CI logs and must not contain real credentials.
Fresh and already-migrated schema probes measure `migrations.Run` separately and verify the resulting migration history.
The `store_open` phase includes connection setup and migration handling; probe results do not establish the migration share of every store initialization.

Phase statistics report sample count, errors, total measured work, median, and nearest-rank p95.
Parent test durations include their subtests, and concurrent package or phase totals are not elapsed wall time.
Command wall time includes compilation and execution, but not preceding image builds.
Docker-contained Workflow, recovery, upgrade, deployment, and development-image gates are outside this capture scope, as are individual SQL operations and unfinished fixture phases.
Do not subtract these samples from wall time and label the remainder as test-body time.

## Complete backend qualification

```sh
mise run test-docker-integration
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-microsandbox
```

The Microsandbox gate requires a Linux/amd64 Docker host with usable KVM.
On that host, obtain the group with `stat -c '%g' /dev/kvm`.
From macOS, use an existing authorized remote `DOCKER_HOST=ssh://USER@HOST` and that host's numeric KVM group.
Most source gates transfer fixtures over stdin rather than mounting client-side files.
Image-only deployment smoke must run on the daemon host because Compose secrets are host files.

| Focus | Docker task | Microsandbox task |
| --- | --- | --- |
| Runtime/storage/resources | `test-integration` | `test-microsandbox-integration` |
| Pi protocol/configuration/Sessions | `test-pi-integration` | `test-pi-microsandbox-integration` |
| Complete controlled Workflow | `test-pi-workflow` | `test-pi-microsandbox-workflow` |
| Failure recovery | `test-pi-recovery` | `test-pi-microsandbox-recovery` |
| Upgrade and retained state | `test-pi-upgrade` | `test-pi-microsandbox-upgrade` |
| Image-only deployment | `test-pi-deployment` | `test-pi-microsandbox-deployment` |

`mise run test-compose` delegates to disposable Pi deployment smoke.
Its fixtures use isolated names and remove their own resources; inspect the task/script before running a full-stack gate against a shared host.
The production Compose files retain fixed defaults and are not isolated merely by changing the project name.

The native integration gate also runs the bounded catalog longevity/multi-process, coordinated payload, and v0.7.6-to-v0.7.11 catalog migration/backup-recovery regressions.
See [Microsandbox upgrade evidence](../operators/microsandbox-upgrade.md#ci-evidence-boundary) for their assertions and limits.

## Live external gates

The GitHub gate performs a Developer App repository-ID read and submits both Reviewer outcomes against a pre-existing Developer App-authored Pull Request.
It can mutate live review state.
Supply the `OMNIGREX_LIVE_GITHUB_*` values listed by the skipped test, using a designated fixture repository and Pull Request, then run:

```sh
mise run test-live-github
```

For a paid provider prompt through the production runner, see [live-provider smoke](runtime-qualification.md#live-provider-smoke).
Live gates are opt-in and separate from deterministic controlled-provider CI.
Never substitute real credentials into ordinary fixture gates.

## Release evidence

CI runs `docker-check`, `development-qualification`, and the required Linux/amd64 KVM `microsandbox` job independently.
The stable `check` job succeeds only when all three pass, preserving the required branch-protection context and gating publication on every qualification.
The development job builds a same-checkout golden image, verifies the cold/offline module source, and runs the real development-container unit suite with its resource limits.
To check the dependency contract locally, run `mise exec -- go test -race ./.github/workflows`.
CI runs these tests explicitly because `go test ./...` excludes hidden directories.
They execute the aggregate shell against success, failure, cancellation, skipped, and missing results, and check isolated development-build ordering and failure propagation with disposable stubs.
After publication, CI repeats image-only smoke against actual published registry digests.
The development image has additional exact-digest native qualification before repository promotion.
Passing unit tests or fixture aliases alone does not establish a deployable release.
See [runtime qualification](runtime-qualification.md) and [release selection](../operators/installation.md#1-select-a-qualified-commit).

## Reviewer CI-admission regressions

Use `TestReviewScheduling*` for deterministic completion/grace/deadline evaluation and `internal/reviewpolicy` tests for strict policy validation.
`TestReviewerAllocationRequiresControllerAdmission` exercises the real PostgreSQL allocation boundary without an admission fence.
`TestReviewGateSharedObserverPersistsAndFencesTargets` uses the production shared CI observer with a deterministic HTTP provider, durable healthy delayed claims, frozen policy/inventory, and verified test-merge replacement.
Scheduling fixtures must not sleep for registration grace or CI deadlines.

`TestPiControlledWorkflow` includes pending-CI admission after the actual Developer handoff, asserting no Reviewer Participant, active Turn, successor execution job, or second Runtime Process is allocated until CI becomes terminal.
It then exercises completed-with-failure admission through the real Reviewer launch and native review path.
The existing default Docker and Microsandbox Workflow script selectors both include that scenario.
Run `mise run check`, `mise run test-integration`, and `mise run test-pi-workflow`, and inspect the same-head Linux/KVM CI result for `mise run test-pi-microsandbox-workflow` before claiming backend qualification.
Never attempt nested Microsandbox qualification inside a Microsandbox Execution Environment.

`TestPiFailureRecovery/clarification-restart` records a real completed Pi prompt and question publication for each Role, then restarts the controller Store/backend before settlement.
It verifies corroboration enters clarification waiting without spending an infrastructure retry, stops the Runtime Process, and schedules retained-Session continuation only after a correlated human reply.
The PostgreSQL clarification recovery matrix additionally covers empty-question yields, input on both sides of the corroboration boundary, unpublished-question rejection, and revalidation after explicit Human Handoff recovery.

## Manual release designation

See [release publication](releases.md) for candidate evidence, finite artifact retention, manual alpha publication, and no-rebuild stable promotion.
Run `mise exec -- go test -race ./.github/workflows` explicitly: the hidden-directory suite validates workflow boundaries; ordinary Go release behavior tests live in `scripts/release/` and are included in the normal unit gate.
Product post-publication smoke now verifies and extracts the CI deployment archive before testing its Compose files with captured exact digests.
Dedicated release App setup and live historical-tag authorization, first publication, anonymous GHCR access, and first promotion remain separately authorized post-merge acceptance.

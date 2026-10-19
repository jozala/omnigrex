# Docker execution boundary implementation

## Scope and prerequisite

This records Step 04 of the [Pi and pluggable execution-backend plan](2026-10-05-pi-and-pluggable-execution-backends.md).
Its prerequisite is the [Step 03 boundary design](2026-10-07-runtime-boundary-contracts.md) and decisions 0005-0009.
OpenCode remains the active agent runtime.

## Implemented boundary

- `internal/runtime/execution` defines execution keys, opaque resource references, logical attachments, process requests, lifecycle ports, inventory observations, creation reservations, and normalized uncertainty/conflict/unsupported-requirement errors.
- `internal/runtime/opencode` compiles the existing Runtime Profile and Role configuration into a neutral request.
- `internal/runtime/docker` resolves deployment resources, compiles exact security policy, owns Docker labels and SDK operations, attaches transport before starting, and translates recovery inventory.
- `internal/agentturn`, `internal/startup`, `internal/workflowaction`, and `internal/store` use neutral execution identities and observations.
- `internal/runtime/composition` constructs diagnostic adapters; doctor no longer constructs Docker policies or consumes Docker types.
- The dependency regression checks the production core import graph with ordinary and integration build tags.

The supported Docker guarantee is process/container isolation for trusted repositories.
Requests for hardware isolation, nested containers, or unknown guarantees fail before execution.
Production preparation continues to require an already available exact image; the refactoring does not add image pulling.

## Creation and cleanup evidence

Migration 000030 records an immutable execution-key/specification-digest reservation before creation and binds the returned resource reference.
Reservations distinguish reserved, setup-dispatched, and main-dispatched phases, with separate setup identity and stop evidence.
It also renames the existing OOM evidence column to `runtime_oom_resource_ref` without changing its contents.
Provider credential bytes are excluded from the specification digest and are never stored in a reservation.

The Launcher supplies a lease-bound execution ledger to each engine.
Reservation and phase admission use short fenced transactions and the connection already acquired for the fence; their commits finish before external dispatch.
Provisioning and evidence recording execute outside `WithAgentTurnFence`, so they do not acquire a second pool connection while a fenced transaction holds the first.
The existing bounded start fence contains only process start, with no ledger or other Store calls from its callback.
Backend facts can be recorded after fence loss for cleanup, but a stale lease cannot admit a new creation phase.

Same-key retries cannot dispatch another creation when the original effect is unresolved.
A known, still-created resource can be reattached only after revalidating its identity, image, process configuration, ephemeral environment, and security policy.
A running or exited resource is not adopted as an outstanding prompt.

Cleanup first revokes further creation for the exact epoch.
Revoking a previously unseen key inserts a durable tombstone; primary-key conflict handling serializes it with first reservation.
A reserved phase has dispatched no external work, and revocation prevents its transition to setup or main creation.
For a lost creation acknowledgement, the adapter resolves the deterministic name and checks the exact identity and specification digest before recording the immutable resource ID.
An unresolved reservation whose resource cannot be found stays `UnknownEffect`; an empty inventory is not an absence receipt.
An authoritative no-image rejection or a failed setup whose immutable resource was conclusively removed records a settled empty creation instead of leaving a fictitious pending main create.
The setup ID is recorded before starting or deleting the initializer.
A lost setup-deletion acknowledgement can therefore be settled by inspecting that exact ID after restart, even when no initializer remains in inventory.
Unknown setup creation uses its deterministic setup name, independently of the main-creation name and phase.
Main creation requires confirmed setup stop and a separately committed, live-fenced phase transition; revocation denies that transition.
After removing every exact-key resource, cleanup additionally verifies that the bound immutable ID is absent.
Delayed starts against that removed ID cannot recreate the environment, and a revoked reservation cannot dispatch a replacement create.
Revoked reservations remain recovery barriers rather than being deleted during ordinary process cleanup.

Startup classification separates unauthorized physical resources from matching durable executions.
When the Store cannot authorize a full inventory identity, the orphan-cleanup port removes only its observed immutable resource IDs after revalidating management ownership and every identity field, including the Runtime Profile.
That port does not read or mutate the execution ledger, so an orphan sharing a legitimate Turn/epoch cannot revoke its reservation or insert a conflicting tombstone before first reservation.
Matching durable identities still use the revocation and phase-recovery barrier; the ledger's ownership-conflict checks remain strict.

Subpath preparation resources carry the execution identity so restart cleanup can discover them.
Diagnostic probes instead carry a separate, expiring management identity and use disposable storage.
Live diagnostics are excluded from Agent Turn recovery; expired diagnostics are revalidated before removal.
Signed-epoch overflow is classified consistently by inventory and malformed-resource cleanup.

Missing, running, unstarted, and unavailable process observations do not establish an exit or OOM cause.
Exit code 137 alone does not establish OOM.
Positive OOM evidence is still saved under the live or recovered Turn fence before resource removal.
Setup resource OOM facts are not attributed to an Agent Runtime Process that has not started.

## Verification coverage

| Requirement | Evidence |
| --- | --- |
| Exact image, policy, isolation, and capability validation | Existing Docker policy tests and `TestExecutionCompileRejectsUnsupportedRequirementsAndUnpinnedImages` |
| Same-key retry and changed-request rejection | `TestExecutionConformanceDocker`, reservation persistence tests, and request-digest tests |
| Ambiguous creation and cleanup barrier | `TestExecutionCleanupKeepsAmbiguousCreationUnknownUntilResourceIsFound` |
| One-connection pool and durable admission before dispatch | `TestExecutionLauncherUsesSingleConnectionPool` exercises the real Launcher and Store, independently observes committed dispatch intent, and uses the same pool during provisioning |
| Lost setup deletion and restart | `TestExecutionRecoversLostSetupDeletionAcknowledgement` and `TestExecutionSetupIdentityAndPhaseSurviveStoreReopen` |
| Revocation before or racing first reservation | `TestExecutionRevocationFencesPreviouslyUnseenKey` and `TestExecutionFirstReservationRacingRevocationCannotDispatch` |
| Phase admission after lease loss | `TestExecutionAdmissionChecksLiveFenceBeforeEachPhase` |
| Known no-effect creation and interrupted setup | `TestExecutionKnownMissingImageHasNoUnresolvedCreation`, `TestExecutionKnownSetupFailureDoesNotLeaveUnresolvedCreation`, and `TestExecutionResolvesSurvivingSetupWithoutAttributingItsOOMToAgent` |
| Ambiguous start | `TestLostStartAcknowledgementRequiresEnvironmentStop` |
| Startup stdout, framing, separate stderr, and bidirectional transport | Actual controlled process in `TestExecutionConformanceDocker` |
| Detachment differs from stop; environment background work terminates | Controlled process remains running after transport closure, then has confirmed exit after stop |
| Active OpenCode through the neutral boundary | `TestExecutionOpenCodeUsesNeutralFactoryAndTransport` initializes OpenCode 1.18.29 and creates a disposable Session |
| Restart recovery and immutable ownership | Store reservation reopen/revocation tests and existing startup recovery integration tests |
| Exact-epoch, duplicate, malformed, and failed cleanup | Existing startup and exact-cleaner suites, plus late-create and epoch-overflow regressions |
| Orphan cleanup preserves a live execution with another Runtime Profile | `TestExecutionOrphanCleanupPreservesLegitimateProfileReservation` uses real PostgreSQL and Docker with an existing reservation and before first reservation |
| Cleanup regression is isolated from other daemon workloads | The reconciler receives an immutable fixture-ID allowlist across unique, duplicate, and malformed inventory entries; unrelated managed sentinels remain running with unchanged start times |
| Inventory isolation covers mixed duplicate groups and malformed resources | `TestExecutionOrphanInventoryScopesEveryCategory` filters every category and normalizes a mixed duplicate group with one owned resource to a unique process |
| Orphan removal is limited to observed IDs and revalidated ownership | `TestOrphanCleanupRevalidatesFullIdentityWithoutRevokingLedger` and `TestOrphanCleanupRemovesOnlyObservedImmutableResources` |
| OOM and unknown exit evidence | Existing live/recovered OOM persistence suites and Docker observation tests |
| Diagnostic lifecycle | Doctor integration and diagnostic inventory/expiry tests |
| Core dependency direction | `TestCoreDependencyGraphDoesNotReachExecutionImplementations` |

The following commands passed on the development Docker Engine with the race detector where applicable:

```sh
mise install
go test -race ./internal/runtime/docker ./internal/runtime/opencode ./internal/runtime/execution ./internal/agentturn ./internal/startup ./internal/workflowaction ./cmd/omnigrex
go test -race -tags=integration -run 'TestExecution|TestRunCommitsEachMigrationAndResumesAfterLateFailure|TestUpgradeFromSchema25PreservesExistingAcceptedReview|TestRunSerializesConcurrentCallsAndReleasesLock|TestMigrationTwelveAppliesFromCurrentSchema|TestRunExecutesMigrationsExactlyOnceUnderConcurrentCalls|TestOpenUsesPasswordSecretAndReturnsReadyStore|TestOpenCodeContinuedSessionUsesCurrentDeploymentInstructions' ./internal/runtime/docker ./internal/store ./internal/runtime/acp -timeout 5m -count=1
go test -race -tags=integration ./internal/store ./internal/runtime/docker -run '^TestExecution' -count=1 -timeout 3m
mise run check
mise run test-integration
```

The complete integration gate covers doctor, OpenCode ACP continuation/replay/qualification, Docker, Store, and deployment tests.
The deliberately destructive Compose gate and live-provider checks are outside this Step 04 verification.

## Transitional ownership and next steps

Logical attachment names still resolve to the existing Participant-local workspace, runtime-state, mise, and tool-data layout inside the Docker adapter.
Step 05 replaces live workspace access with logical workspace/capture operations.
Step 06 replaces path-based retained-state ownership and adds durable backend/storage bindings, including their namespace and version contracts.
Step 07 moves repository tool provisioning into Execution Environments.

Legacy direct Docker/OpenCode qualification fixtures retain EOF-driven shutdown through the private `Reattachable` policy distinction.
The neutral adapter keeps stdin available across pre-launch reattachment and requires explicit environment termination.
Remove that legacy distinction with the remaining OpenCode path in Step 17.
The current OpenCode credential environment and ACP coordination remain transitional until their assigned Pi implementation steps.

Operational procedures remain authoritative in the [operator guides](../operators/README.md).

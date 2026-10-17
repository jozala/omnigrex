# Runtime Transition Background

This archive preserves implementation-specific context removed from current decision descriptions during the Pi documentation cleanup.
The linked decision records remain authoritative for current policy; the statements below describe their original delivery stages.
The implementation-agent attribution used by decisions 0010-0013, 0015, 0017, and 0019 originally read `OpenCode implementation agent`; their current records use the runtime-neutral label `Implementation agent` for the same contributor.

## Execution and agent boundaries

[Decision 0005](../../decisions/0005-separate-execution-backends-from-coordination.md) originally recorded that the existing deployment would remain Docker/OpenCode until the planned cutover.

[Decision 0006](../../decisions/0006-use-pi-through-an-agent-specific-adapter.md) was adopted while replacing OpenCode with Pi.
Its original context contrasted ACP with Pi RPC session identity, prompt completion, replay, and configuration semantics.
Keeping ACP-shaped types would obscure Pi's admission-versus-settlement distinction.
The initial cutover reset OpenCode operational/runtime state instead of converting conversations.
An OpenCode restore after fresh Pi work cannot merge ledgers or undo published GitHub effects.

[Decision 0007](../../decisions/0007-supply-provider-api-keys-through-disposable-files.md) replaced an implementation that injected an OpenCode authentication bundle into each Runtime Process.
It superseded ADR 0010's runtime-specific credential format and injection mechanism.

## Retained state and repository environments

[Decision 0011](../../decisions/0011-bind-retained-state-and-validate-offline-session-archives.md) originally preserved Docker/OpenCode operation while removing physical state paths from coordination.
OpenCode storage was Participant-local and could contain multiple Agent Sessions.
SQL could not infer the configured Docker storage namespace during migration, so an existing installation was explicitly Docker-owned and composition claimed its previously unspecified namespace once.
The original directory layout stayed behind the Docker adapter to preserve operational continuity without conflating storage refactoring with the later Pi cutover.
Legacy path fields and direct OpenCode qualification fixtures remained transitional until runtime removal.

[Decision 0012](../../decisions/0012-qualify-repository-environments-without-rebinding-sessions.md) addressed an earlier OpenCode admission guard that compared new images against historical Participant images even for fresh independent Sessions.
The guard remained only for legacy callers during preparation.
Golden-resource qualification at that stage checked the pinned OpenCode distribution and installed executable.
Retained images predating the packaged bootstrap received that platform executable from the exact deployment golden image through a qualified managed initializer.
The artifact source digest was bound to the execution request, its Turn-local staging directory was mounted read-only, and staging was collected after environment removal.
Provisioning and Session Continuation still used the original pinned image.
The final Pi-only implementation removed that legacy bootstrap route.

## Pi Session integration and qualification

[Decision 0015](../../decisions/0015-reserve-exact-pi-sessions-and-replay-bounded-history.md) kept ACP identity and capabilities separate during preparation-stage compatibility.
Its schema addition did not convert conversations.
Complete Agent Turn composition followed in Step 12, and Step 17 removed the transitional ACP coordinator.

[Decision 0017](../../decisions/0017-bind-pi-qualification-to-backend-contracts.md) introduced backend-scoped Pi evidence while the existing compatibility artifact and immutable SQL rows described OpenCode state without an Execution Backend axis.
Legacy artifacts and rows remained unchanged during preparation to preserve their immutable evidence.
Preparatory Pi contracts without an inventory retained current-bundle validation only until the clean cutover; fresh Pi contracts captured the inventory before production use.

[Decision 0020](../../decisions/0020-finish-pi-only-runtime-and-neutral-outcome-evidence.md) ended the preparatory multi-runtime period.
The temporary Pi integration still translated neutral prompt outcomes into ACP responses and persisted ACP stop reasons.
It removed OpenCode execution, configuration, and build paths and required a clean reset of OpenCode operational/runtime state.
Historical rows were preserved through a forward-only schema change without treating ACP evidence as Pi completion.
The historical reset rehearsal remained available through an operator-supplied old-release bundle and exact source images rather than building the removed runtime from current source.

## Initial environment proof

The 2026-10-05 Microsandbox feasibility guest built OpenCode 1.18.29 and passed `TestReadinessProbeExercisesRealDockerResources` with the race detector.
The [topology evidence](../../experiments/microsandbox.md) retains the backend and nested-container observations separately from that historical runtime build.

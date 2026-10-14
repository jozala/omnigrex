# Pi upgrades and state recovery

## Prerequisite and scope

This implements Step 14 of the [combined plan](2026-10-05-pi-and-pluggable-execution-backends.md), following [Pi failure recovery](2026-10-10-pi-failure-recovery.md).
It applies [backend-scoped qualification](../decisions/0017-bind-pi-qualification-to-backend-contracts.md), [immutable Participant images](../decisions/0009-pin-participant-images-and-use-working-revision-tools.md), and [offline Session archives](../decisions/0011-bind-retained-state-and-validate-offline-session-archives.md).

## Qualification artifacts

Strict version-2 artifacts qualify Pi-to-Pi `pi-rpc/v1` source and candidate bindings, including their full contract hashes and exact registry images.
They bind Linux platform, `pi-session/v3`, `/workspace`, `pi-controlled-state-upgrade` suite/version, canonical UTC timestamp, success, and backend family/contract version.
Installation namespaces remain archive ownership constraints rather than release qualification axes.
Startup and doctor reject mismatched configured targets and backend contracts.

Pi Runtime Profiles also retain a canonical golden-resource inventory binding the approved Node version, platform-specific mise checksum, and SHA-256 checksums of owned Pi files, including the package and dependency lock.
The existing full-contract ledger persists that inventory under the Runtime Profile hash; JSONB key reordering cannot change canonical resolution.
Docker and Microsandbox validate the bound inventory before repository scripts execute, including when reopening a historical source image under a newer deployment.
They do not substitute the current deployment's package bundle or bypass resource checks for retained Sessions.

Forward-only migration 35 adds a separate immutable Pi evidence table, leaving legacy OpenCode evidence unchanged.
Imports are atomic and idempotent; the same exact qualification key with different result content conflicts.
Docker and Microsandbox evidence cannot substitute for one another.
Neither imported evidence nor a current-image change rewrites protected Participant or Agent Session bindings.
Fresh Participants select independent images under decision 0012 rather than migrating historical Session data.

## Shared executable gate

`TestPiControlledStateUpgrade` exercises the same scenario on both backends with real Pi 1.0.3 source and Pi 1.0.4 candidate packages, Node 24.14.1, and the production owned runner, exact Session preparation, RPC, checked configuration, and History Replay.
The previous-package fixture uses its own committed dependency lock and integrity retention rather than a mutable npm install.
The source and candidate images exclude the controlled provider, which is injected only into disposable qualification processes.
Every source, candidate, and restored-source launch additionally passes the production golden-resource qualification path against its own immutable inventory.
Actual registry image inputs therefore qualify those exact images rather than overlay image identities.
Fixture-only aliases are not deployment artifacts.

The gate creates a source Session, settles a real controlled prompt, stops and removes its Execution Environment, and exports the ownership-bound archive.
It restores that archive into separate candidate and replacement-host storage resources without merging or modifying the original.
The candidate and replacement source runtimes reopen the exact Session, replay the original append-order event identities, reject duplicate cursor replay, and continue with the original user context.
Each fresh controlled provider asserts that replay made zero model requests before continuation.
Checked model configuration may append configuration observations during reopening; it cannot append new conversational activity during replay.
The final source export must be byte-for-byte unchanged.

Restores are retryable and reject different Participants, backend contracts, namespaces, state formats, or image bindings.
Credentials are checked absent from archives, and workspaces, tooling, MCP authority, and provider processes are reconstructed rather than retained.
An optional private fixture archive enables the same ownership checks and continuation on a second compatible host with the exact images transferred separately.

## Durable protection and recovery

`TestPiProtectedStateRecovery` creates a complete production Pi Developer Turn, including fenced MCP publication and durable outcome settlement, on each backend.
It verifies the Store's offline writer barrier, backend-confirmed stopped boundary, export through production composition, protected-state collection rejection, relocation under unchanged ledger metadata, idempotent restore, and exact durable Session file identity.
Importing Pi qualification remains idempotent and conflict-detecting without changing the retained Participant's image or hash.
A candidate cannot attach source state, a backend change cannot reinterpret the installation, and missing initialized state fails availability instead of being recreated empty.
The same test resolves a protected source contract while a different candidate is current, verifies actual cached image availability, and confirms that unavailable source-image evidence fails readiness.
The normal Store integration suite independently verifies backend-distinct immutable Pi evidence and migration history.

Session-only archives remain supplemental to PostgreSQL and protected Workspace Captures.
The operator guide is authoritative for [qualification and compatible-host recovery](../operator-guide.md#pi-upgrade-and-state-recovery-qualification), [backup](../operator-guide.md#backup), and [restore](../operator-guide.md#restore).

## Delivery boundary

OpenCode remains operational during preparation; its artifact schema, table, and qualification entry point are transitional until Step 17.
Pre-Step14 preparatory Pi definitions without the new inventory retain current-bundle validation; the explicit clean cutover discards populated preparatory operational state, and new Pi Participants capture their own resource inventory.
This step adds copy-based Pi qualification and recovery evidence, with no live image rebinding or state migration route.
Published Pi images, permanent KVM CI, final selected-runtime operational integration, and clean-cutover rehearsal remain Steps 15-17.

## Verification evidence

Observed passing commands:

```sh
mise exec -- go test ./agent/pi ./internal/runtime/profile ./internal/runtime/environment ./internal/runtime/pi ./internal/runtime/docker ./internal/runtime/microsandbox
mise run check
mise run test-integration
mise run test-pi-upgrade
mise run test-pi-microsandbox-upgrade
OMNIGREX_PI_WORKFLOW_TEST_PATTERN='^TestPiProtectedStateRecovery$' mise run test-pi-workflow
OMNIGREX_PI_WORKFLOW_TEST_PATTERN='^TestPiProtectedStateRecovery$' mise run test-pi-microsandbox-workflow
```

The final upgrade gate passed on Docker/Linux arm64 and native Microsandbox/Linux amd64 with usable KVM, including production golden-resource checks for both package versions.
Protected-state and source-image readiness checks passed on both backends with the production Store, Launcher, and offline composition boundary.
The full Docker integration suite passed with migration 35 and immutable Pi evidence coverage; its opt-in live and destructive Compose gates remained skipped by normal configuration.

A separate physical-host drill exported a Docker/Linux amd64 source Session on the approved Linux host, transferred the exact fixture images and private archive to the local Docker host, and passed the same reopening, replay, and continuation assertions there using Linux amd64 emulation.
The destination preserved Participant, Session, backend, source image, resource inventory, and platform identity.
Microsandbox additionally passed isolated replacement-storage recovery on the KVM host; a second physical KVM host was not used.
These are Session-state recovery qualifications, not a full production PostgreSQL/Workspace Capture restore rehearsal, which remains part of the later operational rehearsal.
Connection values, image aliases, private archives, and deployment metadata are excluded from this evidence file.

Final inspection identified that current-deployment resource checks would invalidate historical Pi package bundles.
The retained inventory and full golden-resource upgrade gate address that defect rather than skipping validation or treating an existing Session as proof of current-resource compatibility.
Unit coverage verifies safe declarative inventories, historical Node/mise/file checks, canonical full-contract recovery, and rejection of unsupported inventories and qualification axes.

## Independent review corrections

The first independent Standards and Spec reviews both reproduced a cross-platform selection defect and an output-publication defect.
Fresh Pi environments now capture the approved resource inventory for their selected platform while preserving the original Runtime Profile unchanged.
Regression coverage exercises both amd64-to-arm64 and arm64-to-amd64 selection.

Qualification outputs now use checked file transfers, shared archive-codec validation in private disposable storage, and atomic exclusive publication on the destination filesystem.
The source-checkout publication helper rejects empty, corrupt, and truncated outputs, preserves existing destinations, and publishes mode-0600 files.
Script-level fault tests reproduce failed copies with both empty and partially emitted content for compatibility artifacts and Session archives.

After those corrections, `mise run check` and `mise run test-integration` passed again.
Both backend upgrade gates passed with archive export enabled, exercising validated exclusive publication of actual completed Session archives.
Fresh second-round Standards and Spec subagents reviewed the complete tracked and untracked Step 14 changes, confirmed both earlier findings resolved, and each reported zero remaining or new actionable findings.
Their independent focused checks included malformed and corrupt archive rejection, publication permissions and exclusivity, historical resource qualification, canonical bindings, and dependency checks.

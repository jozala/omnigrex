# Upgrade an installation

[Operator guide](README.md) · [Backup](backup.md) · [Restore](restore.md)

Read the selected Release notes, breaking changes, and explicit upgrade requirements before every upgrade.
A fresh-install smoke does not prove historical upgrade compatibility; follow a fresh-install requirement when declared, and treat unknown paths as unqualified.
Create a coordinated backup before every application, PostgreSQL, or Runtime Profile upgrade.
Use the same backend, logical namespace, protected images, and selected Compose file set throughout.

## Publication association upgrade precondition

Before upgrading across the publication-evidence change in [decision 0042](../decisions/0042-associate-publication-at-turn-settlement.md), finish or explicitly resolve pre-change publication operations and their related Agent Turn settlement, recovery, and deferred-event reconciliation under the old orchestrator.
Keep the installation quiescent through the orchestrator replacement; do not admit new work that can create pre-change evidence during this boundary.
A successful old-format `open_pr` result lacks metadata needed for the new settlement-time association path and is unsupported there.
Do not rely on a restart, another webhook, or another Agent Turn to enrich it.
If unfinished pre-change processing cannot be resolved, postpone the upgrade and retain its ledger and protected state for operator investigation using [the troubleshooting guide](troubleshooting.md).
This is a manual upgrade precondition, not an automated drain or migration facility.

The updated orchestrator records PR association independently of review readiness and tracks valid heads without resuming a Human Handoff.
Deploying it does not reopen completed settlements, reset failed reconciliation deliveries, repair historical Workflows, or authorize new work in existing Human Handoffs.
Historical repair requires separate authorization; never reset the database or delete retained state to bypass this boundary.

## Orchestrator-only upgrade

An ordinary orchestrator-only upgrade leaves the checked-in Pi bundle selection, installation Pi digest/platform, and repository development-image pins unchanged.
For copyable image/platform settings, use the [qualified source-run guidance](installation.md#3-resolve-and-provision-images) for your selected Release, not the early publication table or the latest built Pi candidate.
These snippets do not replace installation credentials or authorize repository environment adoption.

The authoritative selection is `agent/pi/selected-bundle.json`, also captured in schema-2 release `build.json` as `pi_bundle`; it is independent of the orchestrator commit and unselected checkout runtime candidates.
Compare the selected bundle and all other immutable process-contract changes before treating an upgrade as orchestrator-only.
Retained Participants resolve their exact original canonical contracts from the ledger; do not reconstruct them from the selection.
Record the current commit and images, download and checksum the target Release assets, then stage its common bundle, selected overlay, and environment example, then update the orchestrator digest.
Reconcile configuration changes against [deployment settings](../reference/deployment-configuration.md).

Preserve historical compatibility artifacts as recovery evidence.
Configure `OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE` only when its results target the exact contract constructed by the new orchestrator.
For the 128-to-512 PID-default change within `pi-rpc/v2`, an artifact targeting the earlier contract is stale even if image/platform are unchanged.
Clear the optional import setting or replace it with separately qualified results targeting the exact new contract before deploying.
Clearing import does not delete ledger results, rebind Participants, or authorize migration.
Never edit an artifact's target hash to force a match.

```sh
docker compose config --quiet
docker compose up --wait
```

Startup applies forward-only embedded PostgreSQL migrations under an advisory lock.
There are no down migrations; do not downgrade the database in place.
Rollback after migration requires the coordinated pre-upgrade database/state recovery set and previous application/configuration.

For a PostgreSQL major version change, logically dump and restore into a new volume using the target version.
Do not reuse the physical data volume across major versions.

## Runtime Profile image change

### Normal rollout without Session migration

Establishing the initial explicit baseline is itself a coordinated cutover if existing repository images contain a different formerly checkout-selected runner.
Do not assume the latest published development image or this repository's existing pin satisfies the initial selection.
Qualify and explicitly adopt selected-bundle-derived images before resuming fresh admission; no deployment reset or Session disposal is involved.

New Participants select the newly configured image and start independent Agent Sessions; existing Participants retain their exact original image, platform, and versioned contract.
A normal supported Pi image rollout does not require source-to-candidate Session qualification or generating/importing a Pi compatibility artifact.
This does not waive release qualification, exact image/resource validation, or repository preflight.
See [decisions 0009](../decisions/0009-pin-participant-images-and-use-working-revision-tools.md) and [0012](../decisions/0012-qualify-repository-environments-without-rebinding-sessions.md).

1. Build and qualify the deliberate R2 bundle and all required derived repository images on the selected backends before changing selection.
   Initial fresh production admission is Linux/amd64 only; local arm64 candidates are not qualified production selections.
2. Create the coordinated pre-upgrade [backup](backup.md) and record exact images, contracts, configuration and revision.
   Provision R2 images in the backend before the switch and retain every protected R1 image and its ledger contract.
3. Pause admission of new work using existing operational controls while deploying the reviewed bundle selection and updating installation `OMNIGREX_PI_IMAGE`/`OMNIGREX_PI_PLATFORM` and trusted-default-branch repository image pins.
   Coordinate [repository-owner promotion](../repository-owners/development-environments.md#qualify-and-promote); changing the installation golden image does not update repository pins.
   There is no overlap admitting R1 and R2 for new Participants, no automatic pin repair, and no runtime installation into an old image.
4. Preserve optional compatibility artifacts as historical evidence; clear an obsolete import setting or replace it with separately qualified evidence targeting the new exact contract/backend.
   Clearing import does not delete ledger evidence or rebind Participants; never edit target hashes to force a match.
5. Validate Compose, deploy, require readiness, and run [repository preflight](health-and-diagnostics.md#repository-preflight) for every managed repository before resuming new work.
   A resource mismatch means provision/qualify the correct self-contained image and explicitly promote its pin, not bypass the bounded checks.

Existing R1 Participants continue Sessions and receive new Agent Turns in their original images with unchanged binding hashes, platform, inventory and Agent Session identity.
This includes retained arm64 Participants without a current arm64 production selection.
A Reviewer first created after the cutover requires R2 even when its existing Developer remains on R1.
Do not wait for all R1 Participants to finish merely because an image changed; protect their images until collection.
Actual protocol, Session, API or configuration incompatibilities can impose separate release prerequisites and must be assessed explicitly.
A runtime/repository-image migration belongs in upgrade prerequisites and does not automatically make a release breaking or authorize deleting state.

### Optional copied-state compatibility qualification

Use [Pi upgrade qualification](../contributors/runtime-qualification.md#pi-upgrade-and-state-recovery-qualification) in CI or a separate source checkout when source-to-candidate compatibility evidence is wanted.
Provision actual exact source/candidate registry images and supply the trusted canonical source contract when immutable fields differ, including historical PID budgets.
The gate tests Session Continuation and History Replay on isolated copies, not protected live Sessions.
Results are direction-sensitive and scoped to exact images, platforms, Runtime Profile contracts, state format, and backend family/contract version; they do not prove reverse rollback compatibility.

To import successful evidence, transfer the private JSON artifact to an absolute host path outside the downloaded deployment directory and set `OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE` to that path.
Startup and doctor validate supplied results against the exact configured target contract and backend version; an obsolete optional setting can block deployment even though no artifact is required for the normal rollout.
Importing records compatibility only: it does not authorize or implement live Session migration, Participant rebinding, in-place state conversion, or restoring source state into a candidate-bound Participant.
There is no supported command to perform those operations.
See [decision 0017](../decisions/0017-bind-pi-qualification-to-backend-contracts.md).

### Rollback limits

Do not treat optional qualification, even in the reverse direction, as a supported in-place rollback or Session migration procedure.
The supported safe rollback is coordinated pre-upgrade restore with the previous deployment configuration.
New orchestrators may understand contracts older orchestrators cannot read; preserve a compatible orchestrator for recovery.
See [decision 0029](../decisions/0029-preserve-historical-pid-budgets-and-size-new-participants.md).

## Verify the upgrade

Require [readiness and repository preflight](health-and-diagnostics.md), verify ingress if changed, and run controlled work appropriate to the upgrade.
Check retained image/state availability rather than assuming a healthy new Participant proves old Sessions can continue.
Record the deployed revision, digests, configured contract, qualification artifact if used, and observed outcome privately.


## Conversation contract cutover

The short-reference and explicit-assessment contract in [decision 0062](../decisions/0062-use-short-conversation-references-and-explicit-assessments.md) is a clean compatibility boundary.
Before upgrading to this contract, finish or explicitly stop all active Workflows using existing lifecycle controls.
Do not upgrade while an old-format question, checkpoint or acknowledgment publication is interrupted or uncertain: reconcile it under the old deployment first or explicitly stop the affected Workflow.
Retain historical records, but do not expect UUID commands, reference backfill, old-renderer reconciliation or continuation of old-format publications across the upgrade.
Do not delete history or automatically reactivate completed Workflows.
Evaluate the new contract on fresh Work Items, including a separately authorized post-deployment real-provider conversation assessment; deterministic fixtures alone do not establish tone or semantic interpretation quality.
Within the new contract, publication freezes exact rendered payloads before external effects and retains exact-operation recovery after restart.
An acknowledgment reservation whose POST outcome is uncertain is reconciled against its frozen artifact; absence is not permission to repost, and admission deduplication does not prove visible receipt delivery.
A proven client rejection (for example a rate-limit response) durably releases only that POST attempt, allowing a delivery retry to reserve the same frozen receipt again.
Timeouts, server failures, transport failures and lost responses do not release ownership; stale rejection reports cannot release a later attempt.

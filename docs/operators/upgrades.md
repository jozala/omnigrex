# Upgrade an installation

[Operator guide](README.md) · [Backup](backup.md) · [Restore](restore.md)

Read the selected Release notes, breaking changes, and explicit upgrade requirements before every upgrade.
A fresh-install smoke does not prove historical upgrade compatibility; follow a fresh-install requirement when declared, and treat unknown paths as unqualified.
Create a coordinated backup before every application, PostgreSQL, or Runtime Profile upgrade.
Use the same backend, logical namespace, protected images, and selected Compose file set throughout.

## Publication association upgrade precondition

Before upgrading across the publication-evidence change in [decision 0042](../decisions/0042-associate-publication-at-turn-settlement.md), finish or explicitly resolve pre-change publication operations and their related Agent Turn settlement, recovery, and deferred-event reconciliation under the old controller.
Keep the installation quiescent through the controller replacement; do not admit new work that can create pre-change evidence during this boundary.
A successful old-format `open_pr` result lacks metadata needed for the new settlement-time association path and is unsupported there.
Do not rely on a restart, another webhook, or another Agent Turn to enrich it.
If unfinished pre-change processing cannot be resolved, postpone the upgrade and retain its ledger and protected state for operator investigation using [the troubleshooting guide](troubleshooting.md).
This is a manual upgrade precondition, not an automated drain or migration facility.

The updated controller records PR association independently of review readiness and tracks valid heads without resuming a Human Handoff.
Deploying it does not reopen completed settlements, reset failed reconciliation deliveries, repair historical Workflows, or authorize new work in existing Human Handoffs.
Historical repair requires separate authorization; never reset the database or delete retained state to bypass this boundary.

## Orchestrator-only upgrade

Keep the configured agent digest/platform and protected historical images, but account for possible changes to the default contract selected for new Participants.
Record the current commit and images, download and checksum the target Release assets, then stage its common bundle, selected overlay, and environment example, then update the controller digest.
Reconcile configuration changes against [deployment settings](../reference/deployment-configuration.md).

Preserve historical compatibility artifacts as recovery evidence.
Configure `OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE` only when its results target the exact contract constructed by the new controller.
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

New Participants select the newly configured image and start independent Agent Sessions; existing Participants retain their exact original image, platform, and versioned contract.
A normal supported Pi image rollout does not require source-to-candidate Session qualification or generating/importing a Pi compatibility artifact.
This does not waive release qualification, exact image/resource validation, or repository preflight.
See [decisions 0009](../decisions/0009-pin-participant-images-and-use-working-revision-tools.md) and [0012](../decisions/0012-qualify-repository-environments-without-rebinding-sessions.md).

1. Create the coordinated pre-upgrade [backup](backup.md) and record the deployed revision, configuration, exact images, and canonical contracts.
2. Select a supported qualified release and reconcile its deployment configuration using [installation](installation.md#1-select-a-qualified-commit).
3. Set `OMNIGREX_PI_IMAGE` and `OMNIGREX_PI_PLATFORM` to the new exact digest/platform and [provision it in the selected backend](installation.md#6-start-the-selected-bundle).
   Keep canonical historical Runtime Profile records in PostgreSQL and every protected historical image available until its Participants are collected.
4. If `OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE` is already configured, replace it with separately qualified evidence matching the new exact target contract and backend family/version, or clear the optional import setting before deployment.
   Preserve the old private artifact as historical recovery evidence; clearing import does not delete ledger results or change retained bindings.
   Never edit an artifact's target hash to force a match.
5. Validate Compose with `docker compose config --quiet`, deploy with `docker compose up --wait`, then require readiness and a passing [doctor for every repository](health-and-diagnostics.md#repository-preflight).
   Exact configured/protected image, owned-resource, and retained-state checks remain mandatory.

Repositories explicitly pinning a development image in `.omnigrex/environment.yaml` require [separate image qualification and promotion](../repository-owners/development-environments.md#qualify-and-promote) on their trusted default branch.
Changing the installation golden image alone does not update those pins, even for new Participants.
Retained Participants keep their original binding in either case.

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
New controllers may understand contracts older controllers cannot read; preserve a compatible controller for recovery.
See [decision 0029](../decisions/0029-preserve-historical-pid-budgets-and-size-new-participants.md).

## Verify the upgrade

Require [readiness and repository preflight](health-and-diagnostics.md), verify ingress if changed, and run controlled work appropriate to the upgrade.
Check retained image/state availability rather than assuming a healthy new Participant proves old Sessions can continue.
Record the deployed revision, digests, configured contract, qualification artifact if used, and observed outcome privately.

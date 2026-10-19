# Upgrade an installation

[Operator guide](README.md) · [Backup](backup.md) · [Restore](restore.md)

Create a coordinated backup before every application, PostgreSQL, or Runtime Profile upgrade.
Use the same backend, logical namespace, protected images, and selected Compose file set throughout.

## Orchestrator-only upgrade

Keep the configured agent digest/platform and protected historical images, but account for possible changes to the default contract selected for new Participants.
Record the current commit and images, download the target qualified commit's common bundle, selected overlay, and environment example, then update the controller digest.
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

Existing Participants retain their exact original image and versioned contract.
Source-to-candidate qualification tests isolated Session copies; it does not migrate Participants or prove reverse rollback compatibility.

1. Qualify actual exact source and candidate registry images in CI or a separate source checkout using [Pi upgrade qualification](../contributors/runtime-qualification.md#pi-upgrade-and-state-recovery-qualification).
2. Include the trusted canonical source contract from the source release/recovery set when its immutable fields differ, including historical PID budgets.
3. Transfer the resulting private JSON artifact to an absolute host path outside the downloaded deployment directory.
4. Set `OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE` to that path.
5. Set the candidate `OMNIGREX_PI_IMAGE` digest/platform and provision it in the selected backend.
6. Validate Compose, deploy, and run doctor for every repository.

Results are backend-scoped and direction-sensitive.
Startup and doctor validate them against an exact configured contract and backend version.
Keep canonical historical Runtime Profile records in PostgreSQL and both source/candidate images available until their Participants are collected.
Only new Participants select the current image.

An in-place rollback is unsupported after candidate Participants exist unless the reverse direction is also qualified.
The supported safe rollback is coordinated pre-upgrade restore with the previous deployment configuration.
New controllers may understand contracts older controllers cannot read; preserve a compatible controller for recovery.
See [decision 0029](../decisions/0029-preserve-historical-pid-budgets-and-size-new-participants.md).

## Verify the upgrade

Require [readiness and repository preflight](health-and-diagnostics.md), verify ingress if changed, and run controlled work appropriate to the upgrade.
Check retained image/state availability rather than assuming a healthy new Participant proves old Sessions can continue.
Record the deployed revision, digests, configured contract, qualification artifact, and observed outcome privately.

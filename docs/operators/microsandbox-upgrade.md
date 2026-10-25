# Upgrade the Microsandbox catalog to v0.7.11

[Execution backends](backends.md) · [Backup](backup.md) · [Restore](restore.md)

## Selected candidate and qualification

Work Item [#120](https://github.com/jozala/omnigrex/issues/120) supplies the upstream investigation and immutable pins for v0.7.11, the latest published non-prerelease checked on 2026-10-25 UTC.
This is not authorization to upgrade a running installation or a claim that a particular revision is already qualified.
Select an exact Omnigrex revision only after its `docker-check`, `development-qualification`, `microsandbox`, and aggregate `check` pass.
Post-merge `deploy-smoke-microsandbox` and `development-smoke-microsandbox` remain release/promotion requirements as described in [installation](installation.md#1-select-a-qualified-commit).
Use the checks on that revision, not baseline results or a single healthy Turn.

The coordinated payload is Go SDK/embedded native FFI v0.7.11, the Linux/amd64 runtime image manifest `bdaca423e18beb4a62a6664935de9457c1b3217e0479efce1cc961ee59448734`, and patched external `agentd` built from source `df3e0f7dd69280b88f6e28d6d3cf5fe4bb4a87a0`.
The commit-addressed source archive SHA-256 is `c79dcdfbc262d487dd595e26dab832de3b77f0bc08321ef642a02ff4861d8a41`.
The published Go tag is one bundle-population-only commit later, `19128ecc1425d844a7c96aa455efe8f881313b61`; do not replace the published module with the unpopulated source directory.
The SDK loads its own native library from `MSB_HOME/lib/v0.7.11/`; updating only `msb` does not update a long-running SDK process.
The runtime image supplies the matching firmware at `/usr/local/lib/libkrunfw.so.5.6.1` (kernel 6.12.113).
The existing cgroup patch remains necessary and unchanged; host/guest protocol generation is now 10 rather than 9.

The supplied ancestry and source review establishes inclusion of upstream #1766, first released in v0.7.8.
Linux identity checks use `O_PATH` rather than ordinary database-file open/close, preserving SQLite's process-wide POSIX locks.
This strongly matches the reported incident, but the incident did not capture deleted SHM mappings and is not a conclusively proven diagnosis.
Upstream #1918's interrupted-creation recovery was reverted by #1928 at the selected release tip; do not rely on its guarantees.

## Compatibility and admission barrier

Coordinate every CLI, SDK, diagnostic tool, and orchestrator sharing the catalog.
Stop old processes before allowing v0.7.11 to open it; never mix v0.7.6 and v0.7.11 catalog writers.
Keep explicit local-backend selection, runtime/firmware paths, `MSB_HOME`, `MSB_AGENTD_PATH`, UID/GID 10001, and the original logical storage namespace.
Do not rebind protected Participant images or weaken lifecycle fences, required guest flush, bounded release, or durable cleanup to accommodate the upgrade.

Two catalog migrations follow v0.7.6:

- `m20261001_000001_guest_clock_config` is a compatibility marker; old readers reject the unknown migration.
  The upstream supported downgrade rejects saved desired/active clock policies other than absent, null, or explicit `sync`.
- `m20261022_000001_sandbox_caller_config_overrides` adds nullable `sandbox.caller_config_overrides`, leaving legacy rows null.
  Its down migration drops that column.

A development catalog opened by the unreleased #1918 implementation needs its supported downgrade or a separately isolated home; do not assume production v0.7.6 state must be discarded.
This guide selects coordinated pre-upgrade backup recovery rather than claiming an in-place downgrade is qualified.
Never manually remove migration records, SQLite/WAL/SHM files, volumes, or recovery journals to force compatibility.

## Rehearse and roll out

1. Keep ingress closed and follow [backup](backup.md), including graceful settlement and confirmed native absence.
   Resolve unknown dispatch or missing writeback evidence through unchanged-configuration recovery before taking the backup.
2. Preserve a **full quiescent native home** in addition to the normal PostgreSQL, retained Session, Workspace Capture, and journal recovery set.
   The normal journal-only archive is not a pre-upgrade catalog backup.
   With the old deployment selected and `BACKUP_DIR` pointing to the private coordinated backup directory, run:

   ```sh
   (
   set -eu
   umask 077
   test -d "$BACKUP_DIR"
   test ! -e "$BACKUP_DIR/microsandbox-home.tar.gz"
   docker compose run --rm --no-deps -T --entrypoint tar orchestrator \
     -C /var/lib/omnigrex/msb -czf - . > "$BACKUP_DIR/microsandbox-home.tar.gz.tmp"
   tar -tzf "$BACKUP_DIR/microsandbox-home.tar.gz.tmp" > /dev/null
   mv "$BACKUP_DIR/microsandbox-home.tar.gz.tmp" "$BACKUP_DIR/microsandbox-home.tar.gz"
   (cd "$BACKUP_DIR"; shasum -a 256 microsandbox-home.tar.gz >> SHA256SUMS)
   )
   ```

   Run fail-fast (`set -eu`) in the same stopped-writer window as the other archives; do not proceed after a failed command.
   Protect this archive like credentials: it includes private catalog metadata, cached images, and the native journal.
3. Rehearse migration and recovery on isolated copies under the original in-container paths and namespace, without production ingress, credentials, or host authority over production resources.
   Never point the candidate at the live recovery set merely to inspect compatibility.
   The native CI fixture exercises a v0.7.6 catalog with acknowledged retained volume data, v0.7.11 migration and process restarts, old-reader rejection, and recovery from the untouched pre-upgrade home.
   It does not replace a deployment-specific rehearsal of the complete coordinated recovery set.
4. After separate operator authorization, select the qualified exact native orchestrator image and matching deployment configuration, then start with admission still closed.
   Require readiness, all-pass [repository doctor/preflight](installation.md), and controlled Developer publication, cleanup/recovery, and Reviewer launch before reopening admission.
   Observe operator logs for repeated SQLite, lifecycle-lock, migration, or cleanup failures; retain evidence and close admission rather than deleting state.
5. Keep the pre-upgrade recovery set and exact old images until the rollback window is explicitly closed.
   Record the actual revision, check results, rehearsal results, and post-upgrade work; do not label pending CI or an unperformed live upgrade as successful.

## Rollback and recovery

Simply switching back to v0.7.6 against the migrated home is unsupported: old readers reject its migration history.
If failure occurs, close admission, fence every writer, preserve the candidate state and diagnostics, and obtain operator authorization for recovery.
Use the [restore procedure](restore.md) for the coordinated PostgreSQL, retained Sessions, Workspace Captures, and native journal; additionally restore the **full pre-upgrade native home** into an isolated replacement native volume mounted at the original path before starting any old SDK/CLI.
Keep the upgraded home fenced and preserved; the journal-only restore does not undo catalog migrations.
Select the matching pre-upgrade images and configuration and verify native absence before any state replacement.
If candidate inventory is unavailable, it is not proof of absence: preserve/fence the original host and resolve that uncertainty before recovery.
Reconcile GitHub publications and all acknowledged work since the backup before resuming automation; backup restoration does not undo external effects.
No restore, downgrade, live volume replacement, or running-installation mutation is authorized by the implementation Work Item itself.

## CI evidence boundary

`mise run test-microsandbox` runs the KVM-backed native integration, Pi protocol, controlled Workflow, failure recovery, upgrade, and deployment gates.
`TestNativeCatalogLongevity` adds 16 VM creation/exec/completion/capture/flush/stop/removal cycles, 16 three-second idle gaps, 32 independent SDK processes sharing the catalog, durable cleanup replay and fencing, cross-process acknowledged-volume checks, live `PRAGMA integrity_check`, and deleted WAL/SHM mapping checks in the long-lived SDK process.
Its output reports actual elapsed time and process participation, rather than presenting a test timeout as measured soak duration.
`TestNativeCatalogUpgradeAndBackupRecovery` uses only disposable copies and reports migration/recovery assertions.
`TestNativeCoordinatedReleasePayload` checks the actual CLI, firmware, loaded FFI hashes, local backend, explicit runtime paths, and patched-agent provenance.
The Pi failure-recovery matrix also exercises acknowledged `publish_changes` and `request_review`, an injected unknown cleanup effect, blocked Reviewer admission until confirmed recovery, durable terminal-intent corroboration, and subsequent Reviewer launch without replaying publication.
These are bounded regressions, not a guarantee that every production concurrency schedule or outage is covered.
Omnigrex agents run non-KVM checks locally and obtain native evidence from PR CI; guest `/dev/kvm` presence is not permission or proof of nested virtualization.

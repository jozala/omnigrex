# Manage retained data and capacity

[Operator guide](README.md) · [Documentation hub](../README.md)

## Session retention

Closing an Issue fences new work, settles admitted mutations, completes Participants, and schedules retained Runtime Process state deletion.
`OMNIGREX_ASSIGNMENT_RETENTION_DURATION` defaults to 30 days (`720h`).
Reopening before collection cancels deletion but does not resume automation.
For direct-development Work Items, the user must add `omnigrex:run` after reopening.
Planning Work Items reject that development transition; there is no new completed/closed-planning reopening flow in this increment.
See [planning](../users/planning.md) for stopping and Human Handoff recovery behavior.

Physical collection is irrevocable once started.
After collection, a new trigger creates a fresh Assignment Generation with new Participants/Sessions; prior operational history remains in PostgreSQL.
Protected Workspace Captures are collected with their Participant rather than Turn scratch or live workspace caches.
Host-local [Microsandbox native log archives](troubleshooting.md#retained-microsandbox-native-logs) follow this same Participant retention authorization and are collected with Session data.
Their retention countdown starts after Issue closure, not after an individual Turn finishes or a Change Proposal becomes ready.
There is no separate log retention setting or size quota; monitor disk capacity because complete logs can contain substantial primary-execution output.

## Protect images and bindings

Keep every digest referenced by active or retained Sessions available until corresponding state is collected.
That includes repository-specific environment images, not just the current deployment golden image.
Keep exact registry artifacts readable for native verification and flat-image materialization.
Warm native verification reuses validated persistent registry documents without network requests, but cache misses, flat-image materialization, and guest pulls may still require registry access.
The [document cache](backends.md#provision-exact-images) is reproducible evidence, not protected Session data.

Runtime state binds backend family, contract version, and complete storage namespace when the installation first composes.
Changing those bindings later fails instead of attaching different data.
A missing initialized Participant state directory fails availability and launch checks; it must not start an empty conversation store.
Restore matching state using the coordinated [restore procedure](restore.md).
Do not prune a protected image or rebind a Participant to bypass readiness failures.

## Agent Turn memory

Set `OMNIGREX_AGENT_TURN_MEMORY_MIB` to a positive MiB value in the deployment `.env`.
The supplied Compose stacks default to 1024 MiB, while the binary's unset fallback is 512 MiB.
Recreate the orchestrator and run doctor after changing the setting:

```sh
docker compose up -d --force-recreate orchestrator
```

New Turns and runtime probes use the new allocation; running environments are not resized.
This setting does not change existing immutable Runtime Profile bindings or require a database reset.
At concurrency two, the bundle default permits up to 2 GiB across execution environments, in addition to VMM overhead and host services.

## Tool-cache monitoring

Repository [tool paths](../repository-owners/tool-paths-and-caches.md) have no per-Participant disk quota, like workspace storage.
Monitor backing storage separately from memory capacity.
The background cache monitor measures only Participant assignment-lifecycle caches, stored under historical `assignment-<ID>` prefixes.

`OMNIGREX_ASSIGNMENT_TOOL_CACHE_POLL_INTERVAL` defaults to `5m`.
`OMNIGREX_ASSIGNMENT_TOOL_CACHE_WARNING_MIB` defaults to `1024` MiB.
At the threshold it logs `agent_participant_id`, `size_bytes`, and `threshold_bytes` once per unchanged size.
Warnings never block Turns, enforce a quota, or change retention/cleanup behavior.

Measurement counts allocated blocks, including directories, read-only across bounded resumable scans.
At most four active scans retain 32 directory handles and 8192 hardlink identities each.
Collection, cache replacement, fatal error, completion, and shutdown release handles.
Unreadable/unsafe entries and depth or hardlink limits produce partial measurements, which cannot clear warnings based on a below-threshold partial sum.
Use the warning to investigate growth rather than deleting active cache directories manually.

## Release evidence

Unreleased CI candidate artifacts expire after 14 days and then cannot be directly designated as an alpha.
Published alpha and stable assets retain the exact deployment payload and evidence needed for promotion, independent of Actions retention.
There is no automatic Release/asset or runtime-image deletion; preserve exact images required by retained Participants.
See [release operations](../contributors/releases.md).

## Complete Participant storage collection

Future collection covers Participant-local mise installations, dependency/build caches, orphan Turn scratch, workspace/publication copies and owned staged/retired copies, in addition to protected Sessions, restore staging, native logs and captures.
See the [storage ownership map](../plans/2026-11-27-participant-storage.md#ownership-map) for configured roots and exclusions.
Already-completed collections are not replayed: historical leftovers remain operator-managed, with no backfill command or startup sweep.
Existing scheduled deadlines, including older 30-day deadlines, are unchanged.

Collection runs automatically after its original deadline under the existing irrevocable authorization.
Missing paths succeed, but unknown ownership, aliases, unexpected mounts, foreign owners, cancellation, inaccessible paths and resource-budget exhaustion prevent completion.
New filesystem categories use bounded no-follow traversal and repair only owned directory permissions.
Host preparation, publication and initialization writers share physical exclusion with collection, including inherited locks in trusted Git subprocesses.
Before enabling this collector, drain and stop every prior-version orchestrator and confirm its host-side Git children have exited.
Deploy the writer and collector changes together on all orchestrators sharing these volumes; do not run mixed versions.
The backing filesystem must provide shared advisory file locks and Linux mount-identity reporting, and the orchestrator must own the configured directories.
The installation-owned `.participant-storage-locks` control files intentionally remain; never delete or replace them while an orchestrator or Git child might be using them.
Do not manually recreate old-Participant directories after collection begins.
Fresh Assignment Generations use different identities.

Look for `Participant storage collection` records with `agent_participant_id`, `category`, `status`, `reason`, `allocated_bytes_observed_removed` and `measurement`, and the separate retained-state outcome.
Allocated bytes are observations of deleted directory blocks and last file links, not apparent sparse sizes or a guarantee of physical release.
Shared extents, external hardlinks and open handles can delay or prevent physical reclamation; retained-state byte measurement is explicitly unavailable.
A partial result is never proof of full collection.
Transient failures use existing collection retries; exhausted retries enter the existing Human Handoff recovery flow.
After interruption, resolve permission/ownership/mount problems through the normal operator recovery procedure and retry the existing Work Item, rather than editing SQL or marking the generation collected.
Unknown entries and depth limits require investigation; do not rename them into a recognized category to bypass ownership validation.

Removed toolchains/caches regenerate only for fresh Participants or during their normal working-revision tool provisioning, potentially requiring registry/package-network access.
Do not prune exact images required by retained Sessions to make space.
Workflow, Participant, Session and collection history remains intentionally retained in PostgreSQL.

## Native image maintenance inventory

Run on the orchestrator with the same deployment configuration and database permissions as normal operation:

```sh
omnigrex image-maintenance --limit 1000
omnigrex image-maintenance --json --limit 1000 --pin registry.example/environment@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
```

Supply each explicitly provisioned or pinned image with another `--pin`; the current deployment image is included automatically.
The command validates the existing backend/namespace binding and reports bounded uncollected Participant image roots, including COLLECTING generations.
It does not open the native image cache, contact a registry, or mutate runtime artifacts.
Normal database initialization/migration prerequisites still apply.
Exit 0 means the inventory command ran, **not** complete native provenance or reclamation.
Read `complete: false`, `apply_supported: false` and every unsupported/incomplete dimension in JSON, or the equivalent text output.
No resource is classified as safe to delete from absence in this inventory.
A limit-exhausted report remains incomplete and all omitted roots remain protected.

`omnigrex image-maintenance --apply` always exits nonzero before resource access with the missing capability reason.
Do not substitute `msb image prune`, force removal, or direct cache-file deletion for that refusal.
The [tracked native GC dependency](../dependencies/microsandbox-safe-image-gc.md) describes the pinned API evidence, required concurrency contract and exact-commit Linux/KVM qualification matrix.
This fallback deliberately does not reclaim the observed orphan flattened blobs.

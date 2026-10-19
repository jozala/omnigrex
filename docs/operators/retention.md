# Manage retained data and capacity

[Operator guide](README.md) · [Documentation hub](../README.md)

## Session retention

Closing an Issue fences new work, settles admitted mutations, completes Participants, and schedules retained Runtime Process state deletion.
`OMNIGREX_ASSIGNMENT_RETENTION_DURATION` defaults to 30 days (`720h`).
Reopening before collection cancels deletion but does not resume automation.
The user must add `omnigrex:run` after reopening.

Physical collection is irrevocable once started.
After collection, a new trigger creates a fresh Assignment Generation with new Participants/Sessions; prior operational history remains in PostgreSQL.
Protected Workspace Captures are collected with their Participant rather than Turn scratch or live workspace caches.

## Protect images and bindings

Keep every digest referenced by active or retained Sessions available until corresponding state is collected.
That includes repository-specific environment images, not just the current deployment golden image.
Keep exact registry artifacts readable for native verification and flat-image materialization.

Runtime state binds backend family, contract version, and complete storage namespace when the installation first composes.
Changing those bindings later fails instead of attaching different data.
A missing initialized Participant state directory fails availability and launch checks; it must not start an empty conversation store.
Restore matching state using the coordinated [restore procedure](restore.md).
Do not prune a protected image or rebind a Participant to bypass readiness failures.

## Agent Turn memory

Set `OMNIGREX_AGENT_TURN_MEMORY_MIB` to a positive MiB value in the deployment `.env`.
The supplied Compose stacks default to 1024 MiB, while the binary's unset fallback is 512 MiB.
Recreate the controller and run doctor after changing the setting:

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

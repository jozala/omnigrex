# In-Turn default-branch snapshots

`refresh_default_branch` is a controller-mediated local Git-data mutation available to both Developer and Reviewer Roles, before or after a Pull Request exists.
It does not publish, merge, reset, clean, replace a Session, reload repository guidance, or certify mergeability.
Repository, branch, workspace, and execution identity come from verified Turn scope; the only argument is an idempotency `operation_id`.
The controller verifies repository identity and uses short-lived Developer/Orchestrator App authority for acquisition, including when a Reviewer calls it.
Credentials and repository configuration from acquisition never enter the Runtime Process.

## Use the returned snapshot

A result identifies the scoped repository and branch, the acquired commit `sha` and usable `ref` when installation completed, and initial and final observation times.
The ref is operation-specific under `refs/omnigrex/default/` and does not update `origin/<default>`, HEAD, index, or existing refs.
The snapshot includes complete ancestry for local `merge-base` and `merge-tree` checks.
A Developer can subsequently merge it under existing permissions; a Reviewer must keep the frozen reviewed head and source files unchanged.

| Status | Meaning and action |
| --- | --- |
| `current` | The post-install observation matched the acquired branch/SHA; locally verify the exact returned snapshot against the Change Proposal head. |
| `stale` | The branch advanced during the call; the returned acquired snapshot remains usable, but request another observation for final freshness. |
| `default_branch_changed` | The repository's default-branch name differs from immutable Turn context; do not silently follow it or rebind authority. |
| `unavailable` | Acquisition or observation could not establish freshness; the bounded `reason` identifies the failed phase, and any supplied ref is not current-base evidence. |

A snapshot is not a promise that the branch cannot move after observation.
Stage guidance requires a fresh observation before Developer handoff or Reviewer approval, repeating local verification whenever either the checked head or base SHA changes.
Replay of the same operation returns its original recorded result and timestamps, not a new observation.
Use a new operation ID when freshness is needed; there is no fixed Stage call-count limit beyond the existing Turn budget.

## Limits, failures, and recovery

Acquisition uses a fresh controller-owned bare repository, strict object validation, SHA-1 pack/index checksum validation, and a complete non-shallow fetch.
Individual pack and index payloads are limited to 256 MiB; the trusted Git subprocess also has a bounded output-file size and a two-minute acquisition deadline.
The complete call has a three-minute deadline, with thirty seconds for installation and a separate bounded ten-second resumption attempt.
Workspace validation streams at most 1 GiB and 200,000 archive entries; packed-ref inspection is limited to 16 MiB, and Git configuration inspection to 1 MiB.
Repositories exceeding these limits are explicitly unavailable rather than partially successful.

Installation quiesces writers, validates destination ancestry, and adds only absent pack/index/ref files, or verifies identical existing bytes.
Only the ordinary files reference backend is supported.
Reftable storage and Git configuration includes/extensions are rejected before any files are installed rather than advertising an unusable loose ref.
It rejects symlinked or hardlinked Git metadata, alternate/common object stores, replacement refs, grafts, shallow history, active Git lock files, corrupt payloads, and conflicting destinations.
Do not bypass a rejection with credentialed Git in the active checkout or rerun workspace preparation.
Let an ordinary concurrent local Git command finish before making a new call; unsupported or unsafe metadata requires existing blocker handling, not destructive repair by the tool.

The backend acquires its execution lock before the controller guards its exact execution reservation against cleanup/revocation.
Live Turn authority is checked before and after installation without holding lease rows, allowing execution heartbeats and MCP authority renewal to continue during delivery.
Cleanup waits for installation/resumption to finish; successor admission still requires confirmed prior execution stop.
See [installation fencing](../decisions/0049-exclude-execution-cleanup-without-locking-turn-leases.md).

The mutation ledger pins replay to its original execution identity and prevents an uncertain operation from fetching a newer base.
Lost HTTP responses can replay a completed result without reinstallation.
Interrupted acquisition/installation or failed resumption is not successful verification, even if some additive bytes are visible.
An ambiguous installation remains unresolved through existing mutation recovery and may require Human Handoff; partial files are never treated as a successful receipt or silently overwritten.
Restart does not reinterpret a local ref as durable capture evidence or rebind publication, CI read targets, or Reviewer authority.
See [operator troubleshooting](../operators/troubleshooting.md) for existing execution/storage recovery procedures.

The new capability is discovered from the current controller catalog through the existing Pi MCP extension; compatible retained Sessions keep their original Runtime Profile binding.
Native Docker and Microsandbox qualification, including controlled-provider Pi discovery, runs in the existing backend gates described in [testing](../contributors/testing.md).

[Reference index](README.md) · [Decision](../decisions/0050-deliver-default-branch-snapshots-as-additive-git-data.md)

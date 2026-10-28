---
status: accepted
date: 2026-10-28
decision-makers: [Omnigrex Developer for Work Item 147]
---
# Exclude execution cleanup without locking Turn leases

## Context

Review of [Work Item 147](https://github.com/jozala/omnigrex/issues/147) reproduced lease starvation when snapshot delivery held the existing Turn fence through slow installation.
That fence locks the job, hierarchy, Turn, and slot rows needed by the execution heartbeat.
Delivery must exclude cleanup and successor workspace replacement without stopping renewal of active authority.

## Decision

Refine the installation-fencing mechanism in [decision 0048](0048-deliver-default-branch-snapshots-as-additive-git-data.md), leaving its acquisition, delivery, and replay choices unchanged.
After acquiring its execution lock, the backend invokes a controller-supplied installation fence around pause, validation, delivery, and resumption.
The fence holds a shared lock on the exact, settled, unrevoked main execution reservation.
Both supported cleanup implementations revoke that reservation before stopping or removing the execution; successor admission still requires confirmed prior stop.
The backend-lock-before-reservation-lock order matches Microsandbox cleanup and prevents a lock inversion.

Validate the complete live Turn authority before and after installation with non-locking reads of the same predicates used by the ordinary Turn fence.
Keep mutation admission, heartbeat renewal, completion, and recovery transactions unchanged.
Concurrent authority loss cancels through the existing gateway watcher and cannot produce successful installation evidence.
The reservation guard prevents old execution work from reaching a successor workspace while cancellation and resumption finish.

## Rationale

Merely lengthening a lease or shortening installation would hide starvation and impose another timing assumption.
Releasing every guard would permit cleanup to race delivery.
Guarding execution revocation instead separates physical writer lifetime from renewable logical authority without a new table, lease policy, or backend path in the common interface.

## Consequences

Heartbeats and their MCP authority renewal continue during slow installation, including while cleanup waits.
Ambiguous effects and post-install authority loss retain existing unresolved recovery semantics.
Qualification must cover slow delivery with a real Store, concurrent renewal and revocation, and both backend implementations.

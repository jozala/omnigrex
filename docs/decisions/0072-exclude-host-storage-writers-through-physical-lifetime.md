---
status: accepted
date: 2026-11-27
decision-makers: [Omnigrex Developer for Work Item 218]
---
# Exclude host storage writers through their physical lifetime

## Context

The collection audit for [Work Item 218](https://github.com/jozala/omnigrex/issues/218) found two pre-reservation writers: staged Git checkout acquisition and retained-state initialization.
The existing short Turn fences protect promotion/admission, but do not span every host filesystem write.
A stale preparation callback could therefore outlive its active Turn and overlap final collection.
Holding heartbeat-dependent database rows through slow Git I/O would violate [decision 0051](0051-exclude-execution-cleanup-without-locking-turn-leases.md).

## Decision

Use Participant-keyed shared physical file locks for host storage writers and an exclusive lock around the complete retention cleaner composition.
Validate durable uncollected admission after acquiring a shared lock; COLLECTING and COLLECTED identities cannot enter new writers.
Cover workspace staging, publication/reconciliation, capture staging, tool-path preparation/cleanup and retained-state initialization.
Preserve native reservation/revocation and confirmed-stop checks for backend execution writers.

Keep lock inodes under the trusted workspace root's installation-owned `.participant-storage-locks` directory and never unlink them during collection.
Pass the live lock descriptor to trusted Git subprocesses, retaining exclusion if the parent closes its descriptor before a child exits.
Use no-follow owner/mount validation for lock metadata, cancellable acquisition, and no long-lived database lock or reserved query-pool connection.
Standalone library fixtures without configured durable admission retain their existing explicit-caller-authority contract; production configures both admission and collection exclusion.

## Rationale and consequences

An application mutex cannot exclude another orchestrator process, and a database lease alone does not pin a filesystem writer after connection loss.
A short promotion fence cannot cover earlier staged writes.
Persistent lock identity avoids splitting exclusion across two inodes when a late caller opens the same Participant key.
The small installation control files remain intentionally, rather than being claimed as reclaimed Participant content.
All orchestrators must share the configured backing paths and deploy this writer/collector contract together, after prior-version orchestrators and their Git children have stopped.
This is not a lock for installation-wide native image GC and gives no authority over external native clients.

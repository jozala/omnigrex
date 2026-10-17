---
status: accepted
date: 2026-10-16
decision-makers: [OpenCode implementation agent]
---
# Preserve historical PID budgets and size new Pi Participants for development

## Context

The development environment runs compilers, a guest Docker daemon, PostgreSQL fixtures, and multiple containers within one aggregate PID budget.
The original Pi ceiling of 128 is insufficient for the orphan-cleanup integration fixture's simultaneous sentinel containers.
PID limits are already part of the full immutable Runtime Profile contract and content hash, so changing a retained Participant's limit would rewrite its identity.

## Decision

New Pi Runtime Profiles use a 512-PID ceiling.
Within the supported `pi-rpc/v2` baseline, continue accepting exactly the earlier 128-PID and current 512-PID contracts, with all other resource and process fields still strictly validated.
Pre-baseline `pi-rpc/v1` contracts and guest paths remain unsupported under [decision 0024](0024-establish-a-clean-supported-release-baseline.md).
Resolve retained Participants from their stored canonical definitions and preserve their original limits and hashes.
The launcher copies the selected contract's limit without a mutable deployment override.

## Rationale

512 leaves headroom for real development workloads while retaining a bounded aggregate ceiling and the existing VM memory limit.
A fixture-only increase would hide failure of the ordinary agent configuration.
Disabling PID enforcement or moving containers outside the shared budget would weaken the resource contract.
A new YAML field or mutable override is unnecessary because canonical contracts already encode and distinguish the limits.

## Consequences

Existing Participants remain at 128 and may need a fresh Assignment Generation to use the development environment's larger budget; promotion alone never rebinds them.
Older controllers reject newly persisted 512-PID contracts, so rollback requires a controller that understands both approved budgets.
An optional startup compatibility file targeting the old budget must be unconfigured or replaced with newly qualified exact-target evidence; preserve the historical file and imported ledger rows.
Historical source contracts must be supplied explicitly when qualification is intended to exercise the old budget rather than today's constructor defaults.

## Links

- [Immutable Participant selection](0009-pin-participant-images-and-use-working-revision-tools.md).
- [Full canonical contract retention](0012-qualify-repository-environments-without-rebinding-sessions.md).
- [Aggregate guest cgroup budget](0028-keep-a-domain-parent-for-nested-containers.md).

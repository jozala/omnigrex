---
status: accepted
date: 2026-10-25
decision-makers: [Work Item owner]
---
# Coordinate Microsandbox payload and catalog upgrades

## Context

Work Item #120 selects v0.7.11 using supplied upstream release, ancestry, artifact, API, and migration evidence.
The v0.7.6 production SQLite failures strongly match upstream #1766, but the incident is not a conclusively proven diagnosis.
A CLI-only upgrade leaves the orchestrator's embedded native library and patched guest payload unchanged.
New catalog migrations also make an unqualified image-only rollback unsafe.

## Decision

Upgrade the published Go SDK/embedded FFI, runtime CLI/firmware image, and patched external `agentd` together to v0.7.11 with immutable pins.
This updates decision 0013's version selection and decision 0028's patched-source version without changing their SDK boundary, POSIX bindings, or cgroup policy.
Preserve explicit local runtime configuration and all existing lifecycle fencing and writeback requirements.
Qualify catalog longevity, cross-process persistence, migration, and coordinated pre-upgrade backup recovery on disposable state in the native PR gate.
Use that gate as authoritative KVM evidence when implementation runs inside an Omnigrex guest.
Select backup recovery rather than claiming in-place downgrade is qualified.

## Rationale

v0.7.8 first contains the lock fix; the Work Item's reviewed v0.7.11 candidate also includes intervening lifecycle and security corrections.
Updating only the CLI or retaining the old agent payload is not a coordinated protocol/library upgrade.
A single successful Turn cannot demonstrate preservation of acknowledged catalog writes across independent process closes.
Switching an old image onto the upgraded catalog fails its migration compatibility contract.

## Consequences

All catalog users must be stopped and upgraded together.
Rollback requires operator-authorized restoration of the full pre-upgrade native home and the coordinated ledger, Session, and Workspace Capture recovery set, with reconciliation of subsequent external effects.
The unreleased interrupted-creation behavior from upstream #1918 was reverted in #1928 and is not a guarantee of this candidate.
Passing a bounded CI regression is evidence for a particular revision, not authorization for a live rollout or proof of every production concurrency schedule.

## Links

- [Work Item #120 and supplied research](https://github.com/jozala/omnigrex/issues/120).
- [SDK and POSIX bindings](0013-use-the-microsandbox-go-sdk-and-posix-bindings.md).
- [Domain parent cgroups](0028-keep-a-domain-parent-for-nested-containers.md).
- [Operator upgrade and recovery](../operators/microsandbox-upgrade.md).

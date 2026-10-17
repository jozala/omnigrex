---
status: accepted
date: 2026-10-16
decision-makers: [OpenCode implementation agent]
---
# Keep an empty domain parent for guest workloads and nested containers

## Context

The development environment's native qualification found that memory-limited Docker containers fail beneath `/microsandbox-workload` after ordinary nested builds.
Microsandbox 0.7.6 places exec processes directly in that parent, preventing domain-controller delegation and causing a threaded subtree that cannot provide per-container memory limits.
The parent also owns Omnigrex's aggregate workload PID budget and Microsandbox's workload freezer, so moving containers outside it would weaken existing guarantees.
The same placement remains in upstream 0.7.8.

## Decision

Package a narrowly patched 0.7.6 guest agent in the Microsandbox control-plane image using its supported `MSB_AGENTD_PATH` override.
Build it from a checksum-verified immutable upstream commit, its locked dependencies, a pinned Rust builder, and the repository-owned patch.
Place SDK exec processes in `/microsandbox-workload/agent`, while retaining the empty `/microsandbox-workload` domain parent for the freezer and aggregate PID limit.
Docker and BuildKit create sibling cgroups beneath that same parent and can delegate domain controllers normally.
Keep the SDK, CLI, firmware, Session format, and Participant image bindings unchanged.

## Rationale

A container sibling outside the budget or split PID pools would change enforcement and capacity semantics.
Disabling memory limits or weakening the integration tests would hide the operational failure.
A Docker downgrade does not repair the populated-parent topology, and an SDK version upgrade alone does not contain the required fix.
The supported guest-agent override avoids maintaining a whole runtime or firmware fork.

## Consequences

The control-plane image owns a small downstream runtime patch that must be rebased and requalified on upstream upgrades.
Qualify memory-limited containers before and after a nested build, subsequent SDK execs, parent domain emptiness, shared PID ancestry, and whole-environment termination.
This runtime compatibility correction is separate from image-local dependency caching, which still requires no runtime cache-copy mechanism.

## Links

- [Backend and POSIX binding policy](0013-use-the-microsandbox-go-sdk-and-posix-bindings.md).
- [Development image delivery plan](../plans/2026-10-16-development-image.md).
- [Upstream workload placement](https://github.com/superradcompany/microsandbox/blob/09df3d4b9d832adaede1fb9a198cfc660bfab8cd/crates/agentd/lib/workload.rs).

---
status: accepted
date: 2026-10-09
decision-makers: [Implementation agent]
---
# Use the Microsandbox Go SDK and POSIX-faithful local bindings

## Context

The feasibility experiments qualified the Microsandbox CLI, while Step 08 requires a recoverable production adapter.
Mariusz questioned the initial CLI implementation, prompting direct qualification of the matching Go SDK.
The SDK provides correlated process events, stable sandbox identities, identity-safe lifecycle handles, and paginated inventory.
The original capture experiment used guest-stat virtualization, which makes host metadata differ from guest-visible metadata.

## Decision

Use Go SDK 0.7.6 for provisioning, inventory, process transport, required-flush pause/resume, and identity-safe termination.
Keep SDK types inside the concrete adapter and select the local SDK backend explicitly.
Use the CLI only for host version/doctor checks and operator or qualification image provisioning.
Build the Microsandbox-enabled orchestrator with CGO and the `microsandbox` tag in its pinned native-runtime image.
The existing static Docker build rejects Microsandbox selection rather than falling back.

Use directory bindings with `stat-virt=off`, matching guest and control-plane UID/GID 10001.
Qualify executable modes, symlinks, captures, and Session archives against this literal POSIX representation.
This refines decision 0010's implementation assumption without changing its guest-visible metadata requirement.

Use cached layered images for ordinary execution and disposable golden-resource diagnostics.
Nested-container requirements select a flat ext4 root, whose exact digest-pinned image may require bounded SDK materialization from its registry.
Image metadata availability remains an admission prerequisite; no mutable image selection, automatic image build, or backend fallback is introduced.

## Rationale

CLI-only feasibility is insufficient justification for losing the SDK's typed exit events, stable resource identities, and pagination.
Explicit native packaging keeps the Docker deployment independent of KVM and SDK shared libraries.
Literal POSIX metadata avoids maintaining a second guest-stat archive decoder and lets both local adapters use the same validated Session archive implementation.
Flat materialization is necessary for the qualified private Docker daemon; a layered image's metadata cache does not prove a flat root artifact exists.

## Consequences

Backend-owned durable dispatch records supplement Store admission and prevent replaying a started or revoked epoch after restart.
Transport detachment is separate from confirmed whole-environment stop.
Guest PID budgets apply to the SDK workload cgroup and nested-container descendants, not host VMM thread counts.
Guest-root access through the private Docker daemon can alter guest controls and read supplied provider credentials; hardware isolation does not mediate those credentials.

## Links

- [Execution Backend ownership](0005-separate-execution-backends-from-coordination.md).
- [Workspace capture contract](0010-seal-portable-workspace-evidence-before-publication.md).
- [Step 08 implementation and qualification](../plans/2026-10-09-microsandbox-backend.md).
- [Operator guide](../operator-guide.md#microsandbox-execution-backend).

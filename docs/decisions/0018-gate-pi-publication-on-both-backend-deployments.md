---
status: accepted
date: 2026-10-11
decision-makers: [OpenCode implementation agent]
---
# Gate Pi publication on both backend deployments

## Context

Step 15 requires deployable, diagnosable Pi environments on Docker and Linux/KVM Microsandbox while preserving the OpenCode deployment until cutover.
The existing operational wiring unconditionally constructs OpenCode and publishes only Docker/OpenCode images.
Source-checkout qualification alone does not prove that a deployed image contains its required runtime resources or works without a build context.

## Decision

Allow a deployment registry containing only Pi, only OpenCode, or both during preparation, with each configured binding requiring its exact image and platform.
Use Pi for shared operational storage probes when configured and diagnose every configured runtime.
Repository Agent Profiles still select their explicit Runtime Profile; this does not migrate retained Participants or authorize rebinding their Sessions.

Distribute a common image-only Pi Compose bundle and exactly one backend overlay.
The Docker overlay grants daemon management access and the Microsandbox overlay grants KVM access to the native control plane.
Neither overlay adds another backend's host requirements.

Gate release publication on Docker qualification and an ephemeral Linux/amd64 KVM CI job, including shared conformance, Pi lifecycle, and image-only deployment smoke.
Missing or unusable KVM fails the job rather than skipping native tests.
Repeat smoke against the published exact digests before treating the release as deployable.

## Rationale

Deleting OpenCode now would advance the later explicit cutover.
Requiring an unused OpenCode image in a Pi installation would preserve the operational coupling this step must remove.
Separate backend overlays make host authority and native-library requirements explicit while sharing secrets and service configuration.
An ephemeral KVM runner avoids making repository CI depend on the operator's persistent private development host.

## Consequences

GHCR publication and package visibility remain release operations after merging these changes.
Retained backend ownership and exact-image availability remain mandatory.
The operator guide owns deployment and recovery procedures.

## Links

- [Step 15 implementation and evidence](../plans/2026-10-11-pi-operational-integration.md).
- [Execution Backend ownership](0005-separate-execution-backends-from-coordination.md).
- [Participant image policy](0009-pin-participant-images-and-use-working-revision-tools.md).

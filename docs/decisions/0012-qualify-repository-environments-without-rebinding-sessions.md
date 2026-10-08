---
status: accepted
date: 2026-10-08
decision-makers: [OpenCode implementation agent]
---
# Qualify repository environments without rebinding Sessions

## Context

Step 07 implements decision 0009's independent per-Participant image selection and working-revision tooling.
The earlier OpenCode admission guard compares a new image against historical Participant images, even when a fresh Participant creates independent Session state.
Applying that guard to repository environments would require unrelated cross-image Session qualification and obstruct their independent selection.
Custom images also require recoverable contract content rather than reconstruction from mutable deployment defaults.

## Decision

Persist the full canonical versioned repository environment contract atomically with Participant preparation and resolve retained bindings by exact validated content.
Qualify actual golden runtime resources inside a bounded, non-networked backend helper before repository scripts execute.
Check the pinned mise artifact, the pinned OpenCode distribution and installed executable, bootstrap identity, and required executables rather than accepting image ancestry or labels.

Create fresh versioned repository-environment Participants with independent state without requiring historical Session migration evidence.
Retained Participants remain pinned and cannot be rebound by qualification results.
Keep the earlier cross-image candidate guard only for legacy OpenCode callers until Step 17 removes them.

Retained images predating the packaged bootstrap receive only that platform executable from the exact deployment golden image through a qualified, managed initializer.
Bind the artifact source digest to the execution request, mount its Turn-local staging directory read-only, and collect it after environment removal.
Repository provisioning and Session Continuation still execute inside the original pinned image; neither rebinding to a newer image nor restoring orchestrator-side installation is authorized.

Run working-checkout tool installation in the managed non-root environment before protocol initialization, then execute the absolute agent binary with validated activation and admitted control-plane environment entries.
Control-plane path allocations override repository mise activation so declared cache and scratch lifetimes remain authoritative.
Keep tool setup inside the existing execution epoch so deadlines and environment-wide termination include its descendants.

## Rationale

Ancestry-only validation cannot detect replaced runtime resources.
Reconstructing custom contracts from current defaults loses historical requirements and platform identity.
Treating new independent Sessions as state migrations couples unrelated Participants without protecting state they never consume.
Orchestrator-side installation executes repository code on the control plane and cannot support location-independent execution.

## Consequences

Repository images are built and published externally and remain guest code under the selected backend's trust model.
Protected image/state readiness and exact retained bindings remain mandatory.
Pi-specific resources and Session upgrade qualification are delivered by the later planned steps.
The operator guide remains authoritative for image availability and recovery procedures.

## Links

- [Participant image and tooling policy](0009-pin-participant-images-and-use-working-revision-tools.md).
- [Execution boundary contracts](../plans/2026-10-07-runtime-boundary-contracts.md).
- [Step 07 implementation and evidence](../plans/2026-10-08-repository-environments.md).

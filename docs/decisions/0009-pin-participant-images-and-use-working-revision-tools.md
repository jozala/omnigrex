---
status: accepted
date: 2026-10-07
decision-makers: [Mariusz]
---
# Pin Participant images and use working-revision tools

## Context

Repository-specific prebuilt images introduce a choice between picking the current image and preserving an existing Participant's runtime contract.
Participants are created lazily, so the repository image can change between Developer and Reviewer creation.
Separately, the current Reviewer/default-branch mise policy can test a Change Proposal with tools different from those its code declares.

## Decision

Select a prebuilt exact image and platform from trusted default-branch configuration when each Participant is first prepared, and retain that immutable binding.
Mariusz explicitly selected per-Participant pinning rather than one environment image shared by an Assignment Generation.
A newly created Reviewer may therefore use a newer image than an existing Developer in the same Generation.

Install repository tools inside each execution environment from the Turn's actual working revision for both Roles.
This does not change default-branch Agent Profile provenance or give a Change Proposal authority to change its Participant's bound image.
Preserve Reviewer capability restrictions and workspace discard.

Retained Participants keep their images during deployment upgrades.
Qualify candidate Pi images against copies of source state, but do not treat qualification as authorization to rewrite retained bindings or migrate live Sessions.

## Rationale

Per-Generation pinning would make Role environments more uniform, but would prevent a later Participant from selecting the current repository image.
Automatic rebinding would make historical execution identity mutable and introduce state-upgrade/rollback machinery beyond this release.
Working-revision tool provisioning removes the deliberate default-branch toolchain mismatch while maintaining separate policy/configuration authority.

## Consequences

Both Roles use the relevant tool configuration, but identical base images are not guaranteed.
Keep historical images and contract definitions available while their Sessions remain protected.
Unsupported configuration fails explicitly instead of silently upgrading an image or selecting another model.

## Links

- [Contract design](../plans/2026-10-07-runtime-boundary-contracts.md)
- [Participant identity](../adr/0008-separate-stage-assignments-from-agent-participants.md)
- [Agent Profile provenance](../adr/0011-discover-repository-agent-profiles-and-select-separately.md)

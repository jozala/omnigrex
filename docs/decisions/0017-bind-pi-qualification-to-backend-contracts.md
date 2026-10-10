---
status: accepted
date: 2026-10-10
decision-makers: [OpenCode implementation agent]
---
# Bind Pi qualification to backend contracts

## Context

Step 14 requires exact Pi source-to-candidate evidence and compatible-host state recovery.
The existing compatibility artifact and immutable SQL rows describe OpenCode state and have no Execution Backend axis.
Decision 0009 already requires retained Participants to keep their exact images and limits candidate qualification to copies of source state.

## Decision

Use a strict version-2 compatibility artifact for Pi, identifying exact source and candidate Runtime Profile bindings, platform, `pi-session/v3`, `/workspace`, qualification suite/version, successful outcome, timestamp, and backend family/contract version.
Keep legacy OpenCode artifacts and rows unchanged during the preparatory period.
Persist Pi results atomically in a separate immutable table keyed by all qualification axes, with a canonical result checksum detecting conflicting evidence.

Capture the approved Node version, mise checksum, and owned Pi file inventory inside each immutable Pi Runtime Profile contract.
Persist and restore that canonical inventory with the existing full-contract ledger, and validate resources against the bound inventory on every launch.
Do not reconstruct historical resource checks from the running deployment's bundle or skip validation merely because a Session already exists.

Exclude the backend's installation namespace from release qualification identity.
Retain that namespace, Participant ownership, state format, image, and Runtime Profile hash in Session archives and validate them on restore.
Compatible-host recovery changes physical storage location while preserving these bindings.

Qualification uses disposable Execution Environments and separately restored archive copies.
Successful evidence does not authorize rebinding a retained Participant, restoring source state into a candidate-bound Participant, in-place state conversion, or cross-backend migration.
Fresh Participants continue selecting their own independent repository environments under decision 0012.

## Rationale

Sharing backend-independent qualification evidence could conceal transport, filesystem, or lifecycle differences between Docker and Microsandbox.
Including installation namespaces in release evidence would instead require repeating identical qualification for every deployment without proving another runtime property.
Changing the legacy table's identity and constraints would unnecessarily alter existing immutable OpenCode evidence.
Current-bundle validation would reject valid historical Pi images after a runtime-package or resource upgrade, while skipping validation would lose corruption detection.
Applying a historical-image guard to fresh independent Pi Sessions would reintroduce the coupling rejected by decision 0012.

## Consequences

Startup and doctor reject supplied Pi evidence targeting another configured image, platform, or backend contract.
Historical images and protected state remain availability prerequisites even after a candidate qualifies.
Preparatory Pi contracts without an inventory retain the current-bundle validation path only until the clean cutover; fresh Pi contracts capture the inventory before production use.
The operator guide remains authoritative for qualification, backup, restore, and image retention.

## Links

- [Step 14 implementation and evidence](../plans/2026-10-10-pi-upgrades-and-state-recovery.md).
- [Participant image policy](0009-pin-participant-images-and-use-working-revision-tools.md).
- [Offline Session archives](0011-bind-retained-state-and-validate-offline-session-archives.md).
- [Independent repository environments](0012-qualify-repository-environments-without-rebinding-sessions.md).

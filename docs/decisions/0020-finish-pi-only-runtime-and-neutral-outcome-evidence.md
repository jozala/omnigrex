---
status: accepted
date: 2026-10-13
decision-makers: [Implementation agent]
---
# Finish Pi-only runtime selection and neutral outcome evidence

## Context

Step 17 ends the preparatory multi-runtime period established by decision 0018.
The accepted delivery plan requires Pi-only support, retained Docker and Microsandbox backends, and an explicit clean reset of OpenCode operational/runtime state.
The preparatory Pi implementation still translated neutral prompt outcomes into ACP responses and persisted ACP stop reasons.
Protected-state checks alone cannot enforce a clean reset when old Participant state has already been collected.

## Decision

Require Pi in deployment configuration, Runtime Profiles, and explicit repository `runtime: pi-rpc/v1` selections.
Remove OpenCode execution/configuration/build paths and accept only backend-scoped Pi qualification artifacts.
Reject any unsupported runtime history in the installation ledger before recovery or admission, including collected history and orphaned contracts.

Consume neutral prompt outcomes directly and persist only admission, status, and limit evidence needed for settlement.
Preserve historical rows through a forward-only schema change without reinterpreting ACP evidence as Pi completion.
Route unsupported pending corroboration evidence to the existing fenced prerequisite Human Handoff.
Retain immutable Pi image/resource bindings and tool-storage paths, including their historical spelling.

Keep historical clean-cutover rehearsal available through an operator-supplied old-release bundle and exact source images.
Current source must not build the removed runtime for rehearsal.
Source qualification does not authorize release publication or production cutover.

## Rationale

A permanent compatibility adapter would retain the runtime coupling the replacement is intended to remove.
Reconstructing historical outcomes or silently accepting an old populated ledger would violate the reset boundary and invent evidence.
Changing already bound Pi storage paths would create unnecessary retained-state incompatibility.
Externally selected old-release artifacts preserve a reproducible reset while keeping the current application Pi-only.

## Consequences

This supersedes decision 0018's preparatory runtime-selection policy while preserving its mandatory both-backend publication and exact-digest smoke gates.
Final isolated live dogfooding and the released clean-cutover/recovery gates remain separate completion requirements.

## Links

- [Pi adapter ownership](0006-use-pi-through-an-agent-specific-adapter.md)
- [Preparatory deployment and publication policy](0018-gate-pi-publication-on-both-backend-deployments.md)
- [Step 17 implementation and evidence](../plans/2026-10-13-pi-only-final-qualification.md)
- [Operator-controlled clean cutover](../operator-guide.md#one-time-opencode-to-pi-cutover)

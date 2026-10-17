---
status: accepted
date: 2026-10-13
decision-makers: [Implementation agent]
---
# Finish Pi-only runtime selection and neutral outcome evidence

## Context

The deployment supports Pi on Docker and Microsandbox while keeping coordination independent of protocol-specific responses.
Prompt settlement requires neutral admission, completion, and failure evidence.
Protected-state checks alone cannot establish installation-wide runtime compatibility when Participant state has already been collected.

## Decision

Require Pi in deployment configuration, Runtime Profiles, and explicit repository runtime selections.
The supported release baseline and contract version follow [decision 0024](0024-establish-a-clean-supported-release-baseline.md).
Accept only backend-scoped Pi qualification artifacts.
Reject any unsupported runtime history in the installation ledger before recovery or admission, including collected history and orphaned contracts.

Consume neutral prompt outcomes directly and persist only admission, status, and limit evidence needed for settlement.
Preserve historical rows through forward-only schema changes without reinterpreting unsupported evidence as Pi completion.
Route unsupported pending corroboration evidence to the existing fenced prerequisite Human Handoff.
Retain immutable Pi image/resource bindings and tool-storage paths.

Source qualification alone does not establish release publication or deployment qualification.

## Rationale

Runtime-neutral outcomes keep protocol translation inside the agent adapter.
Reconstructing unsupported outcomes or silently accepting incompatible runtime history would invent evidence.
Changing already bound Pi storage paths would create unnecessary retained-state incompatibility.
Exact published-image tests qualify the delivered application and resource bundle.

## Consequences

This supersedes decision 0018's runtime-selection policy while preserving its mandatory both-backend publication and exact-digest smoke gates.
Live Workflows and coordinated state recovery require separate qualification.

## Links

- [Pi adapter ownership](0006-use-pi-through-an-agent-specific-adapter.md)
- [Deployment and publication policy](0018-gate-pi-publication-on-both-backend-deployments.md)
- [Step 17 implementation and evidence](../plans/2026-10-13-pi-only-final-qualification.md)

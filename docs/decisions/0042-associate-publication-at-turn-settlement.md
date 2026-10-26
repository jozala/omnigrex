---
status: accepted
date: 2026-10-26
decision-makers: [Mariusz]
---
# Associate publication at Turn settlement without granting readiness

## Context
A Developer can open a Pull Request and publish further commits before its initial Agent Turn blocks, fails, or is interrupted.
Deferring association until `CHANGE_PROPOSAL_READY` loses that publication identity and causes valid deferred synchronization to be rejected.
Associating while the Turn is active would change the snapshot without changing its frozen PR/head authority.

## Decision
Persist versioned, bounded repository/PR identity, refs, opening head, base SHA, and lifecycle evidence with successful `open_pr` operations, using the same representation for normal and recovered opens.
Derive the initial Workflow association from the exact authorized publication ledger inside the existing fenced settlement transaction after mutation admission is closed and outcomes are terminal.
Apply this boundary to live settlement, interrupted recovery, and terminal-corroboration settlement or handoff.
Attach the association only after validating and clearing the original active Turn in the reducer; retain its original PR scope, expected head, epoch, and control revision.
The latest proven published head is tracking metadata, not readiness or permission to resume work.
Existing infrastructure retry and Human Handoff policies remain the only authorities for successors.
Retain post-open publication edges in settlement provenance so existing causal replay can validate delayed or missing webhook observations without rewinding the head.

## Rationale
This extends decision 0022 without broadening active-Turn authority or trusting webhook markers as publication authority.
A separate association projection preserves terminal-outcome validation: blocked or failed observations still cannot carry readiness evidence.
No new worker or GitHub lookup is needed at settlement.

## Consequences
Versioned processing survives restart and exact settlement replay atomically with its jobs.
Pre-change incomplete publication evidence is unsupported, not guessed, enriched, or rewritten.
Upgrades require finishing or explicitly resolving pre-change publication operations and their related Turn settlement, recovery, and deferred reconciliation before deploying this change.
Completed historical Workflows, failed deliveries, and Human Handoffs are not automatically repaired or resumed.
Design acceptance is not deployment or qualification evidence; delivery and observed verification belong to the linked Work Item and Change Proposal.

## Links
- [Work Item 116 and its revised compatibility scope](https://github.com/jozala/omnigrex/issues/116).
- [Decision 0022](0022-track-heads-without-resuming-work.md).
- [ADR 0001](../adr/0001-separate-visible-workflow-and-execution-state.md), [ADR 0003](../adr/0003-orchestrator-owns-session-control.md), and [ADR 0004](../adr/0004-mediate-agent-capabilities-through-mcp.md).
- [Operator upgrade precondition](../operators/upgrades.md#publication-association-upgrade-precondition).

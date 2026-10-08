---
status: accepted
date: 2026-10-07
decision-makers: [Mariusz]
---
# Track Change Proposal heads without resuming work

## Context
A human push to the current Change Proposal while its Workflow was in Human Handoff had its synchronization rejected, so a later explicit activation scoped its new Agent Turn to the stale stored head and the Turn could not hand off.
Stopping autonomous work must not require retaining stale metadata, while resuming work must never rely on a stale observation.

## Decision
Accept synchronization for the durable current Change Proposal in `NEEDS_HUMAN` as tracking-only: update the stored head, clear mismatched readiness, preserve the Human Handoff reason, resume Role, Stage, budgets, Assignment state, and `omnigrex:needs-human` label, and schedule no Agent Turn, Workflow Attempt, or successor.
Verify every accepted activation that has an existing Change Proposal through the orchestrator GitHub client before Turn creation, using the same path for Issue and current Change Proposal triggers: resolve the durable current association, confirm repository and Pull Request identity, open status, head branch, and base branch, accept a changed commit head, and repair the stale stored head transactionally.
Treat closed or merged Pull Requests, identity changes, branch or base changes, and replacement Pull Requests as actionable Human Handoffs without adopting, reopening, or retargeting.

## Rationale
Tracking-only synchronization follows the first-iteration rule that a returning Developer starts from the current Pull Request head and the review-limit rule that Human Handoff stops autonomous work, without conflating head bookkeeping with authorization to resume.
Reverification at preparation reuses the existing preparation job retry, rate-limit handling, revision and association checks, and Turn fencing instead of adding a second retry system or a background sweep, and it repairs histories whose synchronization was previously rejected or never delivered.
The rejected alternative was allowing synchronization in Human Handoff to schedule work directly, which would resume automation without an explicit human command, and the rejected alternative of trusting webhook payload heads was discarded because Issue triggers carry no Pull Request revision and payloads cannot prove current GitHub state.

## Consequences
Pushes during Human Handoff stay current with no autonomous successor, duplicate deliveries stay harmless, stale chains cannot rewind, and a deferred push followed by a blocked Turn updates tracking without resuming.
Explicit activation after a missed or rejected synchronization creates a fresh Turn scoped to the current GitHub head without requiring another push, across `NEEDS_HUMAN`, `PR_READY`, and reopening, while transient GitHub failures retry within the preparation budget and permanent failures hand off without launching a stale Turn.
Racing observations before commit are discarded and retried, Turns never mutate authority after creation, and prior terminal-intent recovery still requires matching heads.
Historical rejected events and completed Turns are unchanged and deployment alone starts no work; stuck Workflows recover on the next explicit human activation.

## Links
Work Item [issue 71](https://github.com/jozala/omnigrex/issues/71); extends [decision 0005](0005-activate-from-current-change-proposal.md) and [ADR 0001](../adr/0001-separate-visible-workflow-and-execution-state.md).

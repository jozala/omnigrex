---
status: accepted
date: 2026-11-06
decision-makers: [Mariusz]
---
# Use permanent short references and explicit agent assessments

## Context

[Work Item 176](https://github.com/jozala/omnigrex/issues/176) records Mariusz's agreed conversation design following real-provider feedback.
UUID-bearing commands and repeated full drafts make discovery unnecessarily mechanical.
Friendly wording must not weaken the collaboration and exact-publication boundaries established by decisions [0055](0055-serialize-work-item-clarification-at-turn-boundaries.md), [0058](0058-retain-paired-planning-checkpoints-in-collaboration.md) and [0059](0059-return-durable-checkpoint-nonpublication.md).

## Decision

Extend the existing collaboration aggregate with permanent Work-Item-scoped round, accepted-input and paired-revision references such as R3, A7 and P2.
Allocate them under the existing Workflow lock, preserve failed reservations, and never reuse or rebind a reference.
GitHub admission still requires an explicit target on a newly created comment's first nonblank line; ordinary comments and edits remain inert.

Agents interpret natural language, including affirmative answers, natural corrections and incompatible human directions.
Every delivered input requires an operation-bound assessment with immutable evidence links before proposal or successful completion in any Role.
Natural corrections explicitly identify earlier input without fabricating an amendment or replacing unaffected decisions.
Material uncertainty remains blocking outside the current question frontier; the application validates recorded effects, not meaning or human precedence.

Confirmation requires an assessed exact-pair command, never an answer or steering message.
A recognized withdrawal, material objection or commitment change invalidates earlier confirmation without erasing it.
Reconciliation does not restore that authority: require fresh confirmation, and a new pair when documents change.

Use short round introductions and occasional consolidated checkpoints rather than full drafts before each question round.
Each checkpoint retains both complete documents in one comment, behind a concise entry point and separate expandable sections.
Validate the full rendered payload, including signature and marker; reject oversize publication definitely and actionably, never truncate or split the pair.
Persist the exact payload before posting and reconcile that artifact rather than regenerating prose after restart.
Reserve acknowledgment publication independently of source deduplication so concurrent deliveries cannot independently post receipts.

## Rationale

Target-less answers lack trustworthy reply-parent context on GitHub.
Truncated UUIDs move collision management to humans; keyword-based agreement, semantic conflict detection and last-answer-wins would wrongly move interpretation into admission.
A new conversation engine would create another ordering authority without improving durability.

## Consequences

Operators must finish or explicitly stop active Workflows before upgrading and evaluate the contract on fresh Work Items.
There is no UUID-command compatibility, historical reference backfill or old-renderer recovery promise across this cutover.
Preserve historical records and never automatically reactivate completed work.
Controlled tests establish mechanisms, not human conversational quality; post-deployment real-provider assessment remains a separate delivery obligation under Work Item 176.

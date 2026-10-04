---
name: omnigrex-decision-records
description: Record or review consequential Omnigrex product, engineering, operations, Workflow, and architectural decisions. Use when choosing a non-obvious policy or tradeoff, revisiting an existing decision, or reviewing decision records.
---
# Maintain the decision log

## Decide whether a record is useful

Record a choice when future contributors might reasonably question or reverse it and its rationale is not obvious from the implementation.
Examples include instruction precedence, when a Workflow waits for a human, and deployment compatibility policy.
Routine refactoring, progress updates, test results, and restatements of existing decisions belong in code, issues, or plans instead.

Read `docs/decisions/README.md`, relevant existing records, and applicable legacy ADRs under `docs/adr/` before adding a record.
Keep one authoritative record per decision; link existing decisions rather than copying them.
Use `CONTEXT.md` terminology.

## Capture the actual decision

Create `docs/decisions/NNNN-short-title.md` using the next available number and the template below.
Aim for 150-400 words, omit empty optional sections, and keep each full sentence on one physical line.
Explain credible alternatives and why they were rejected rather than transcribing the discussion.
Name actual decision makers and link available evidence; never invent approval or consensus.
Use `proposed` for an unsettled choice and `accepted` for an established decision within the decision maker's authority.
Accepted does not mean implemented; link the Work Item or plan for delivery status.
Do not turn routine implementation choices into a new human-approval requirement.

```markdown
---
status: proposed
date: YYYY-MM-DD
decision-makers: [Actual decision maker]
---
# Short statement of the decision

## Context
What question needed an answer, and which constraints mattered?

## Decision
What option was chosen or is proposed?

## Rationale
Why this option rather than the credible alternatives?

## Consequences
What benefits, tradeoffs, and follow-up obligations result?

## Links
Related Work Item, plan, PR, or earlier decision.
```

Add the record to the index with its status and date.
For a reversal, write a new record, link the superseded record, and update the earlier record's status and index entry to `superseded` with a link to its replacement.
Preserve the earlier rationale instead of rewriting history.
Use `rejected` for a considered proposal that was not adopted and include a revisit condition only when it adds useful information.

## Review

Check that the outcome follows from the stated constraints, alternatives are represented fairly, and links and status agree.
Identify missing or conflicting decisions with concrete consequences, not a blanket demand for more documentation.
A Reviewer uses this procedure to assess records and report findings; it does not grant file-editing authority.

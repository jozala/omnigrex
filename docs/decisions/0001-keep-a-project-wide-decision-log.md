---
status: accepted
date: 2026-10-04
decision-makers: [Mariusz]
---
# Keep a project-wide decision log

## Context

Important choices about agent behavior, product scope, operations, and development process can be lost in conversations even when they are not architectural decisions.
The repository already has an architectural decision directory, but it does not cover all of these choices.

## Decision

Use a single index under `docs/decisions/` and short numbered Markdown records for new consequential decisions of any kind.
Keep existing ADRs at their current paths and link them rather than migrating or copying them.
Use a native repository skill as the authoritative writing procedure and template.

## Rationale

A versioned record keeps the decision and its rationale close to implementation and accessible to both people and agents.
One record per decision is easier to reference and supersede than an ever-growing chronological notes file.
The approach follows the lightweight [MADR practice](https://adr.github.io/madr/), which supports decisions beyond architecture.

## Consequences

Record accepted outcomes separately from implementation progress, and preserve replaced decisions as superseded history.
Avoid records for routine implementation details or progress reports.
See the [implementation plan](../plans/2026-10-04-instruction-ownership-and-handoffs.md).

---
status: accepted
date: 2026-10-18
decision-makers: [Mariusz Józala]
---
# Organize documentation around reader responsibilities

## Context

The README prioritizes delivery evidence, and the operator guide combines deployment, repository configuration, everyday GitHub usage, and contributor qualification.
Readers with different responsibilities cannot identify a short, reliable path to their first successful outcome.
Mariusz requested role-separated, navigable documentation and selected GitHub-first Markdown with rewriting and gap filling.

## Decision

Provide separate entry points for users, repository owners, operators, and contributors, linked from a concise project README and `docs/README.md`.
These are human responsibilities, not additions to the Workflow Role catalog.
Keep task-oriented guides within each audience and shared factual contracts in a reference section.
Maintain one authoritative procedure per topic and link across responsibility boundaries.
Keep plans, experiments, decisions, and historical evidence discoverable through supporting indexes, with current instructions clearly distinguished from historical material and future vision.

## Rationale

A single handbook requires unrelated readers to navigate deployment and implementation detail before reaching their own tasks.
A document-type-first hierarchy alone would still require readers to understand the project before choosing an entry point.
A published documentation site could improve search, but is unnecessary for the chosen GitHub reading experience.
Audience indexes with focused Markdown pages provide direct navigation without introducing a site build or duplicating content.

## Consequences

Repository onboarding must explain the owner/operator handoff, and everyday use must be documented independently of installation.
Moving procedures requires updating incoming links and executable documentation tests.
The former operator-guide path remains a relocation index for existing links.
A local link check guards navigation as pages evolve.
Accepted specifications and historical decision rationale remain intact.

## Links

- [Documentation cleanup plan](../plans/2026-10-18-documentation-cleanup.md)
- [Documentation hub](../README.md)

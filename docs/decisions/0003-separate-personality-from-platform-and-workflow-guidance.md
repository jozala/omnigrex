---
status: accepted
date: 2026-10-04
decision-makers: [Mariusz]
---
# Separate personality from platform and Workflow guidance

## Context

Repository Profiles currently mix communication preferences with MCP contracts and assumptions about the next Workflow Stage.
This duplicates platform behavior across repositories and makes a Profile depend on one particular Workflow graph.

## Decision

Keep personality, repository preferences, and skill selection in Agent Profiles, and engineering procedures in repository skills.
Provide platform and Role/runtime constraints from Omnigrex and objectives and completion instructions from the current Stage.
Allow operator common and per-Role guidance outside the repository.
Platform and Workflow contracts remain authoritative; operator Role guidance overrides operator common guidance, and both constrain repository preferences.
This supersedes the instruction-ownership portion of [ADR 0012](../adr/0012-load-repository-skills-from-the-current-checkout.md), without changing its checkout-local skill source.

## Rationale

The source that owns a behavior should own its instructions.
Replacing specific Workflow words with vague handoff wording would hide the coupling rather than remove it.
Code-enforced permissions remain independent of prose precedence.

## Consequences

Set a fixed local Git identity during workspace preparation instead of asking an agent to configure it.
Move mergeability guidance into the Stages now and handle deterministic enforcement separately.
Reviewer guidance explicitly requests native inline comments for findings with valid diff locations.
See the [implementation plan](../plans/2026-10-04-instruction-ownership-and-handoffs.md).

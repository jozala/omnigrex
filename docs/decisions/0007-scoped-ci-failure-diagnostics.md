---
status: proposed
date: 2026-10-06
decision-makers: [Mariusz]
---
# Expose scoped CI failure diagnostics with orchestrator-owned credentials

## Context
Agents could see that CI failed through `get_check_runs` but could not retrieve failing steps, annotations, or logs, which forced a Human Handoff during #45 / PR #47 when Docker-backed tests were unavailable to the Reviewer.

## Decision
Add read-only MCP diagnostics scoped to the Agent Turn repository and head commit: check output with paginated annotations, Actions run and job metadata with steps, bounded job-log excerpts with continuation, and literal log search with context.
Expand Developer App permissions with `actions: read` while keeping Reviewer App permissions unchanged, and fetch signed log URLs through a credential-free download client restricted to approved GitHub storage hosts.

## Rationale
Mediating diagnostics through the orchestrator preserves ADR 0004 and ADR 0009 instead of granting agents direct GitHub or Docker access.
Positive identifiers validated against the scoped head prevent cross-repository or cross-commit access without relying on caller-supplied URLs or check-to-job ID equality.
Bounded excerpts with explicit continuation avoid silent truncation, while a credential-free download boundary keeps installation tokens and signed URLs out of agent-visible errors, operational logs, and traces.

## Consequences
Both Roles can identify the failed step and assertion from a representative failure using only permitted MCP tools, with pending, missing, expired, unavailable, unauthorized, rate-limited, transient, scope-rejected, invalid-continuation, and deadline states distinguishable.
Raw diagnostic text is treated as untrusted data and never copied into operational logs or traces.
Rollout requires approving the expanded Developer App permissions and rerunning preflight before agents receive the new capabilities.

## Links
Work Item [issue 50](https://github.com/jozala/omnigrex/issues/50); ADRs [0004](../adr/0004-mediate-agent-capabilities-through-mcp.md), [0005](../adr/0005-use-separate-developer-and-reviewer-github-apps.md), [0009](../adr/0009-select-github-app-credentials-by-operation.md).

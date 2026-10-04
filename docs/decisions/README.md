# Project decision log

Record consequential product, engineering, operations, Workflow, and architectural decisions here.
Use the [decision-record skill](../../.agents/skills/omnigrex-decision-records/SKILL.md) for the writing process and canonical template.
An accepted decision is not a claim that its implementation has shipped; follow its linked plan or issue for delivery status.

## Decisions

| Record | Status | Date |
| --- | --- | --- |
| [0001: Keep a project-wide decision log](0001-keep-a-project-wide-decision-log.md) | Accepted | 2026-10-04 |
| [0002: Use current-deployment agent instructions](0002-use-current-deployment-agent-instructions.md) | Accepted | 2026-10-04 |
| [0003: Separate personality from platform and Workflow guidance](0003-separate-personality-from-platform-and-workflow-guidance.md) | Accepted | 2026-10-04 |
| [0004: Publish handoff summaries before continuing](0004-publish-handoff-summaries-before-continuing.md) | Accepted | 2026-10-04 |
| [0005: Activate from the current Change Proposal](0005-activate-from-current-change-proposal.md) | Accepted | 2026-10-04 |

## Existing architectural decisions

Existing ADR identifiers and paths remain valid; new decisions use this log.
Do not copy an existing ADR into a second record.

- [ADR 0001: Separate visible Workflow and execution state](../adr/0001-separate-visible-workflow-and-execution-state.md)
- [ADR 0002: Use ACP as the agent runtime boundary](../adr/0002-use-acp-as-the-agent-runtime-boundary.md)
- [ADR 0003: Orchestrator owns session control](../adr/0003-orchestrator-owns-session-control.md)
- [ADR 0004: Mediate agent capabilities through MCP](../adr/0004-mediate-agent-capabilities-through-mcp.md)
- [ADR 0005: Use separate Developer and Reviewer GitHub Apps](../adr/0005-use-separate-developer-and-reviewer-github-apps.md)
- [ADR 0006: Control Docker directly from the orchestrator](../adr/0006-control-docker-directly-from-the-orchestrator.md)
- [ADR 0007: Use code-defined Workflow Definitions and durable Attempt Stages](../adr/0007-use-code-defined-workflow-definitions-and-durable-attempt-stages.md)
- [ADR 0008: Separate Stage Assignments from Agent Participants](../adr/0008-separate-stage-assignments-from-agent-participants.md)
- [ADR 0009: Select GitHub App credentials by operation](../adr/0009-select-github-app-credentials-by-operation.md)
- [ADR 0010: Use one provider credential bundle before proxy mediation](../adr/0010-use-one-provider-credential-bundle-before-proxy-mediation.md)
- [ADR 0011: Discover repository Agent Profiles and select separately](../adr/0011-discover-repository-agent-profiles-and-select-separately.md)
- [ADR 0012: Load repository skills from the current checkout](../adr/0012-load-repository-skills-from-the-current-checkout.md)

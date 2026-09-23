---
status: accepted
---

# Select GitHub App credentials by operation

GitHub credential authority is an operation property rather than a Role property.
The Reviewer Role needs the Developer/Orchestrator App for reads and comments, while native review submission must use the independent Reviewer App.

## Consequences

Role Policy grants tools and maps exceptional tools to credential authorities.
Native `submit_review` execution and reconciliation use the Reviewer App.
All other GitHub reads and mutations use the Developer/Orchestrator App.
Authorization is revalidated at gateway, backend, and recovery boundaries instead of relying only on the tools shown to an agent.
The two App identities and permission contracts remain those specified in [ADR 0005](./0005-use-separate-developer-and-reviewer-github-apps.md).

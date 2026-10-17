---
status: accepted
---

# Load repository skills from the current checkout

The instruction-ownership portion of this record is superseded by [decision 0003](../decisions/0003-separate-personality-from-platform-and-workflow-guidance.md); the checkout-local skill-source decision remains in effect.

Agent Profiles retain Role responsibilities and completion criteria, while native repository skills hold detailed implementation and review procedures.
Profiles that allow the `skill` tool load skills from `.agents/skills/` in the current Agent Turn checkout, including for the Reviewer.
This keeps procedures alongside the code they describe and avoids a separate default-branch skill bundle and its preparation and revision-tracking machinery.

## Consequences

The Pi runner explicitly selects authorized checkout-local skills while disabling uncontrolled project configuration and resource discovery.
Skill loading remains subject to local-tool permission checks and does not grant sub-agent or additional MCP capabilities.
Agent Profiles retain their default-branch provenance under [ADR 0011](./0011-discover-repository-agent-profiles-and-select-separately.md); skill content deliberately follows the prepared checkout instead.
A Change Proposal can therefore alter guidance used during its own review.
[Issue #43](https://github.com/jozala/omnigrex/issues/43) tracks a separate orchestrator-owned human approval gate for skill changes; that gate is not implemented by this decision.

Each new Runtime Process discovers the current skill files, including when continuing an Agent Session that previously loaded older content.
The repository Profiles explicitly require loading their Role skill on each Agent Turn; edits during a running process are not guaranteed to hot-reload.
Checkout-local skills are explicitly authorized repository guidance; they do not enable project extensions or settings.

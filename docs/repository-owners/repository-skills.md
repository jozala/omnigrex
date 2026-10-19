# Add repository skills

[Repository-owner guide](README.md) · [Documentation hub](../README.md)

Skills provide engineering procedures that agents can load when relevant.
Both Roles need `skill: allow` and `read: allow` in their Agent Profiles to use repository skills.

## Create a skill

Store a physical file at `.agents/skills/<name>/SKILL.md`:

```markdown
---
name: repository-example
description: Explain when to use this procedure and what it accomplishes.
---
# Repository procedure

Read the relevant requirements and code before editing.
Run the project's focused checks and report the actual outcome.
```

Use a unique name matching the directory.
Put optional examples or reference files alongside the skill and explicitly explain when to read them.
Select required skills in the Agent Profile, keeping personality and communication preferences there.

The owned Pi runner advertises skill descriptions and uses the permitted `read` tool to load bodies.
Symlink-expanded discovery is rejected.
Uncontrolled project extensions, settings, packages, prompt templates, and automatic context/skill discovery remain disabled.
Skills do not enable sub-agents, change MCP authority, or override platform and Stage contracts.

## Publish skills with their Profiles

Skills come from the prepared working checkout, not the default-branch snapshot used for Agent Profiles.
The Developer may initially use default-branch skills and later use Change Proposal skills; the Reviewer uses the Change Proposal checkout.
New Runtime Processes rediscover skills during Session Continuation; in-process edits are not guaranteed to hot-reload.

Publish required Profiles and skills together.
For an existing Change Proposal, ensure its checkout contains newly required skills before starting or reactivating work.
A required skill that cannot be loaded can cause a blocker and Human Handoff.

A Change Proposal can modify skills used in its own review.
The [human approval gate for skill changes](https://github.com/jozala/omnigrex/issues/43) is planned, not enforced.
See [ADR 0012](../adr/0012-load-repository-skills-from-the-current-checkout.md) and [Pi resource ownership](../decisions/0014-own-pi-resource-loading-with-a-pinned-runner.md) for the rationale.

Next: [Development environments](development-environments.md).

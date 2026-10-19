# Configure Agent Profiles

[Repository-owner guide](README.md) · [Documentation hub](../README.md)

## Create the team files

Publish one Profile for each Workflow Role as a direct lowercase `*.md` child of `.omnigrex/team/` on the default branch.
The initial Workflow uses `DEVELOPER` and `REVIEWER`.
The filename is arbitrary; front matter defines Profile identity and Role.
The current selector requires exactly one discovered Profile per Workflow Role and rejects additional Profiles for that Role.

A Developer Profile can start with:

```yaml
---
name: developer
role: DEVELOPER
runtime: pi-rpc/v2
model: openai/gpt-4.1
permissions:
  read: allow
  edit: allow
  write: allow
  bash: allow
  grep: allow
  find: allow
  ls: allow
  skill: allow
---
Follow the repository's implementation guidance and the current Stage objective.
Explain blockers clearly and report the verification actually performed.
```

Choose a provider/model supported by the selected Pi image and deployment credentials.
The example is not a provider/model allowlist.
For the Reviewer, use a distinct `name`, `role: REVIEWER`, and deny or omit `edit` and `write`.
Give each Profile the reading, shell, and skill permissions its repository procedures need.

See the [field reference](../reference/repository-configuration.md#agent-profiles) for validation, optional thinking levels, and supported local permissions.
Omitted permissions default to deny.
Unknown fields and permissions fail discovery.

The optional `thinking` field accepts Pi's native levels, including `max` where the selected model supports it.
`max` and `xhigh` are distinct: neither is automatically mapped or downgraded to the other.
Preparation and doctor query the actual runtime's supported levels and reject unsupported or silently clamped selections before prompting.

## Write repository preferences

Use the body for repository preferences, personality, communication style, and skill selection.
Put reusable engineering procedures in [skills](repository-skills.md).
Omnigrex supplies platform rules, the current Stage objective, completion criteria, and purpose-specific guidance.

Guidance precedence is platform/Stage contracts, operator Role guidance, operator common guidance, then repository preferences.
Actual capabilities and transitions remain code-enforced.
Local `bash` executes trusted repository code and is not a security boundary against arbitrary file edits.
MCP authority is enforced separately from local permissions.

## Understand updates

Profiles are loaded from the latest default-branch commit before each Agent Turn, with directory listing and every file pinned to that same commit.
A feature branch cannot replace the Reviewer Profile that reviews it.
A file can move without changing identity if its front matter `name` remains unchanged.
Every direct Markdown file is configuration, so do not put a README in `.omnigrex/team/`.

An existing Agent Participant keeps its durable Profile identity and immutable Runtime Profile binding.
A configuration change that conflicts with that binding creates a Human Handoff rather than migrating its Session.
Never put credentials or private endpoint secrets in a Profile.

Ask the operator to run repository preflight after publishing configuration.
Next: [Repository skills](repository-skills.md).

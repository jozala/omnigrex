---
status: accepted
---

# Discover repository Agent Profiles and select separately

Roles and Role Policies are deployment-owned, while Agent Profile identities and mutable configuration are repository-owned.
Every Agent Profile is a direct Markdown file under `.omnigrex/team` whose strict front matter declares a stable `name` and one Role.
The filename is not part of Agent Profile identity, so a file may move or be renamed while preserving its front matter name.

The orchestrator lists and loads every Agent Profile from one exact default-branch commit, validates each declared Role against the deployment Role Policy Catalog, and builds a catalog that may contain multiple Profiles for one Role.
Profile selection is a separate operation from discovery.
The initial selector requires exactly one Profile for every Role referenced by the Workflow Definition.

## Consequences

Role Policies own capabilities, credentials, isolation, and runtime hardening, but never Agent Profile names or repository paths.
Every direct `.md` file under `.omnigrex/team` is configuration, so a malformed Profile or an unknown or unreferenced Role fails the complete discovery operation.
Agent Profile names are local to a repository and must retain one Role within that repository.
Different repositories may use the same Profile name for different Roles.
An existing Agent Participant continues to select its durable Agent Profile name; current Role selection applies only when a Stage has no Participant.
Agent Turns retain the exact source path, commit, content hash, and canonical Profile configuration as provenance.
A future selector may choose among multiple Profiles for one Role without changing discovery, Agent Participant identity, or Stage Assignment persistence.

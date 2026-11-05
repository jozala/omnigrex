---
status: accepted
date: 2026-11-05
decision-makers: [Mariusz Józala]
---
# Confine website publication to a server-owned root

## Context

Website publication uses direct `rsync --delete` as selected in decision 0060.
A configurable destination under a personal SSH account could erase unrelated files if pointed at that account's home directory.
Mariusz requested server-enforced confinement and prepared a dedicated deployment account before authorizing repository changes.

## Decision

Use a dedicated account without sudo or Docker access, with write authority limited to the website document root outside personal home directories.
Keep the account's SSH configuration root-owned and restrict its CI key to a forced `rrsync -wo -munge` command selecting that root.
Revoke the key's unrestricted authorizations and mount the document root read-only in Apache.
Always upload to `:./` and remove the configurable client destination.
Do not use protected rsync arguments, because the restricted wrapper must inspect the requested command.
Retain direct synchronization, deletion of obsolete website files, and the existing nontransactional publication policy.

## Rationale

An exact-path guard in the client catches accidental configuration errors but does not limit a stolen key or changed workflow.
A dedicated account alone limits filesystem permissions but still permits arbitrary commands and changes to any account-writable files.
A forced restricted command, root-owned SSH configuration, and a fixed relative destination constrain both command execution and the publication target without adding a service or release-management machinery.

## Consequences

The key can still replace or delete the entire website, which is its intended authority, but cannot select a personal home directory as its synchronization root.
Server administrators own the destination mapping and must maintain rsync, `rrsync`, permissions, and key restrictions.
The former unrestricted workflow is incompatible with the new account until its invocation and GitHub environment are updated.
Workflow tests must preserve the fixed destination and restricted-command-compatible arguments.

## Links

- [Website publication policy](0060-publish-one-astro-website-with-starlight-documentation.md)
- [Server prerequisites and GitHub configuration](../../website/DEPLOYMENT.md)
- [Website Change Proposal](https://github.com/jozala/omnigrex/pull/180)

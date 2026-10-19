# Connect a repository

[Repository-owner guide](README.md) · [Documentation hub](../README.md)

## Agree on the installation

Ask the operator for the Developer and Reviewer GitHub Apps to install, the supported providers/models, selected backend capabilities, and any image or cache requirements.
Only trusted repositories are supported, including their dependencies, instructions, and build scripts.
Repository code can read the provider key supplied to its execution environment.
Read the installation's [requirements and trust model](../operators/requirements.md) before granting access.

## Grant GitHub access

Install both Apps on the account or organization and grant both access to the repository.
If installation requires organization approval, arrange that approval with the account administrator.
When selected-repository access is used, add each new repository to both installations.

The operator creates and configures the App identities and private keys using the [GitHub App guide](../operators/github-apps.md).
You do not need to create a new pair of Apps for every repository on the same installation.
No personal access token belongs in the repository.

## Prepare the default branch

1. Add one [Agent Profile](configure-agents.md) for each Workflow Role under `.omnigrex/team/`.
2. Publish every [skill](repository-skills.md) required by those Profiles.
3. Ensure the working checkout declares its toolchain and verification commands, for example through `mise.toml`.
4. If needed, publish a digest-pinned [development environment](development-environments.md) and request operator provisioning.
5. If builds need disk-backed scratch or caches, agree on the operator allowlist and add [tool-path configuration](tool-paths-and-caches.md).

Agent Profiles and environment/path configuration are selected from one exact default-branch snapshot before each Agent Turn.
Skills and repository tool installation come from the actual working checkout, which may be the Change Proposal.
Keep required skills available on both paths before reactivating existing work.

## Confirm readiness

Ask the operator to run `doctor --repository OWNER/REPOSITORY` and verify a real Developer App webhook delivery.
All preflight checks must pass before starting work.
Doctor validates configuration and prerequisites but does not prove that the project's complete build or tests pass.

Managed labels are normally provisioned asynchronously when Developer App access is granted.
Wait for `omnigrex:run` to appear.
If it does not appear, ask the operator to inspect [label provisioning](../operators/health-and-diagnostics.md#label-provisioning).
Repositories installed before automatic provisioning, or with deleted idle labels, may need the operator to create the trigger label manually.

Configure GitHub branch protection if CI checks must be mandatory before merge.
Omnigrex's `omnigrex:pr-ready` label does not enforce required CI checks.

Setup is complete when access, default-branch configuration, preflight, webhook delivery, and the trigger label are ready.
Next: [Your first Work Item](../users/first-work-item.md).

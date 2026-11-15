# Configure a repository for Omnigrex

You own the project's Agent Profiles, skills, development environment, and repository access.
An [operator](../operators/README.md) provides the running installation, GitHub App identities, credentials, and execution capacity.
You may perform both responsibilities yourself.

## First setup

1. [Connect a repository](connect-a-repository.md).
2. [Configure its agents](configure-agents.md).
3. Coordinate operator preflight, then follow [Your first Work Item](../users/first-work-item.md).

## Customize the project

- [Repository skills](repository-skills.md): engineering procedures loaded from the working checkout.
- [Development environments](development-environments.md): prebuilt images, required capabilities, and tool installation.
- [Tool paths and caches](tool-paths-and-caches.md): disk-backed scratch and reusable caches.
- [Review scheduling](../reference/repository-configuration.md#review-scheduling): orchestrator-side CI waits or immediate concurrent review.
- [Repository configuration reference](../reference/repository-configuration.md): supported files, schemas, and provenance.
- [Troubleshooting](troubleshooting.md): configuration and onboarding failures.

The current Workflow requires one Developer and one Reviewer Profile.
Creating another Profile does not add a new Workflow Role or alter the code-defined Workflow.

[Documentation hub](../README.md)

## Planning and implementation consent

[Planning and exact-pair authorization](../users/planning.md) are optional; direct `omnigrex:run` development remains available without a Planner Profile.
For planning Work Items, repository write-or-higher permission and an explicit `/omnigrex implement P2` command for the actual current pair are required before implementation.
Repository instructions may distinguish material design commitments from reversible implementation suggestions, but cannot bypass explicit consent, publication confirmation, pending-input fences or current repository-relevance validation.
Unrelated default-branch changes do not automatically invalidate acceptance; the Planner records relevance evidence and material changes require a revised pair and renewed consent.
Configure normal Developer/Reviewer Profiles and CI policy for the downstream handoff; label projection does not grant implementation authority.

Planned implementation can return a material concern to its retained Planner without replacing the Change Proposal.
Keep the Planner Profile available for that continuation; this does not add initial planning to direct-development Work Items.
See [implementation-time replanning](../users/planning.md#replanning-after-implementation-starts) for unchanged-plan resumption, renewed exact-pair consent and loop limits.

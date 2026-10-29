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
- [Review scheduling](../reference/repository-configuration.md#review-scheduling): controller-side CI waits or immediate concurrent review.
- [Repository configuration reference](../reference/repository-configuration.md): supported files, schemas, and provenance.
- [Troubleshooting](troubleshooting.md): configuration and onboarding failures.

The current Workflow requires one Developer and one Reviewer Profile.
Creating another Profile does not add a new Workflow Role or alter the code-defined Workflow.

[Documentation hub](../README.md)

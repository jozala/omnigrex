# Omnigrex documentation

Choose a guide by the responsibility you have right now.
The same person may use several guides.
These audiences are distinct from the Developer and Reviewer agent [Roles](../CONTEXT.md#agents).

| Audience | You are here to… | First outcome |
| --- | --- | --- |
| [User](users/README.md) | Assign work and review results in GitHub | [Complete your first Work Item](users/first-work-item.md) |
| [Repository owner](repository-owners/README.md) | Connect a project and configure its agents | [Prepare a repository](repository-owners/connect-a-repository.md) |
| [Operator](operators/README.md) | Deploy and maintain an installation | [Install Omnigrex](operators/installation.md) |
| [Contributor](contributors/README.md) | Develop Omnigrex itself | [Set up development](contributors/development.md) |

## How the responsibilities fit together

An operator provides an installation, GitHub App identities, provider credentials, execution capacity, and operational maintenance.
A repository owner grants repository access, publishes Agent Profiles and any required skills or environment configuration, and coordinates preflight with the operator.
A user creates Work Items and supervises their Change Proposals in GitHub.
A contributor changes the Omnigrex application and qualifies its behavior.

For a new self-hosted setup, follow **operator installation -> repository onboarding -> first Work Item**.
No shell access to the installation is required for everyday GitHub use.

## Look up a contract

- [Repository configuration](reference/repository-configuration.md): file locations, fields, authority, and lifecycle.
- [Deployment configuration](reference/deployment-configuration.md): settings, defaults, and configuration layers.
- [Commands](reference/commands.md): operational commands, arguments, and exit behavior.
- [Domain language](../CONTEXT.md): the terminology used throughout the project.

## Understand the design and its history

Current task guides describe supported behavior.
Plans, accepted decisions, and test evidence explain intent or a particular revision; they do not independently prove that a release is deployed or qualified.

- [Architecture overview](contributors/architecture.md).
- [Decision log](decisions/README.md), including the index of existing ADRs.
- [Plans and specifications](plans/README.md).
- [Experiments](experiments/README.md).
- [Live dogfooding evidence](dogfooding.md).
- [Project vision](../project-concept.md), including future capabilities.

## Maintain the documentation

Give each procedure one authoritative home and link to it from other audiences.
Use focused task pages with prerequisites, steps, and a recognizable success condition.
Keep current instructions separate from historical evidence and identify planned behavior explicitly.
See [contributor documentation guidance](../CONTRIBUTING.md#documentation) for verification and writing conventions.

[Back to the project](../README.md)

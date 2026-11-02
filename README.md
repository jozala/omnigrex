# Omnigrex

**A self-hosted platform for coordinating software-development agents through GitHub.**

Turn a GitHub Issue into a Change Proposal produced by a Developer agent and evaluated by a separate Reviewer agent.
Omnigrex manages the Workflow, agent permissions, persistent Agent Sessions, retries, and human handoffs while keeping Issues, Pull Requests, and reviews in GitHub.

```text
GitHub Issue -> Developer -> Pull Request -> Reviewer -> Human review
                   ^                           |
                   +---- requested changes ----+
```

## What works today

- A code-defined Developer/Reviewer Workflow started with the `omnigrex:run` label.
- Repository-owned Agent Profiles, model selection, skills, and development environments.
- Separate GitHub Apps for development and independent native reviews.
- Persistent Agent Sessions across disposable execution environments.
- Durable human clarification and explicitly addressed answers, amendments, and steering on the Work Item Issue.
- Bounded retries and Review Cycles, with explicit Human Handoff when automation cannot continue.
- Durable controller-side CI completion waits before review, with repository-owned immediate-review and expected-check policy.
- Docker or Linux/KVM Microsandbox execution, with a Docker Compose control plane and PostgreSQL ledger.

Omnigrex is in active early development toward its first dogfooding iteration.
The current runtime is Pi, and only trusted repositories are supported.
Docker provides container isolation; Microsandbox adds hardware isolation and supports guest Docker, Compose, and Testcontainers.
The [operator requirements](docs/operators/requirements.md) explain credentials, host authority, supported platforms, and the fresh-installation baseline.

The broader [project vision](project-concept.md) includes capabilities beyond this implementation, such as additional agent Roles and adaptive Workflows.
It is not a list of shipped features.

## Get started

| What you want to do | Start here |
| --- | --- |
| Ask agents to work on an already configured repository | [Your first Work Item](docs/users/first-work-item.md) |
| Connect your project and configure its agents | [Repository-owner guide](docs/repository-owners/README.md) |
| Deploy and maintain an Omnigrex installation | [Operator guide](docs/operators/README.md) |
| Build, test, or contribute to Omnigrex | [Contributing](CONTRIBUTING.md) |

If you are self-hosting for your own project, first deploy an installation, then connect the repository, then create your first Work Item.
There is no separate end-user application to install: GitHub is the everyday interface.

### A first Work Item

After your repository owner and operator have completed setup:

1. Open a small GitHub Issue with explicit acceptance criteria.
2. Add `omnigrex:run` to the Issue.
3. Follow the agent comments and linked Pull Request.
4. Review the result when `omnigrex:pr-ready` appears, or respond to the explanation if `omnigrex:needs-human` appears.

For example, start with a narrowly scoped documentation correction or bug fix whose expected result you can verify.
See [Workflow and labels](docs/users/workflow-and-labels.md) for activation, review, closing, and reopening behavior.
Omnigrex does not enforce required CI checks before marking a Change Proposal ready; configure GitHub branch protection for mandatory checks.

## Documentation

The [documentation hub](docs/README.md) provides audience-specific reading paths and reference material.

- [Users](docs/users/README.md): start work, follow reviews, and handle human handoffs.
- [Repository owners](docs/repository-owners/README.md): connect repositories and configure Agent Profiles, skills, and environments.
- [Operators](docs/operators/README.md): install, monitor, back up, restore, and upgrade.
- [Contributors](docs/contributors/README.md): development, architecture, testing, and runtime qualification.
- [Reference](docs/reference/README.md): repository configuration, deployment settings, and commands.

## Contributing

Omnigrex is written in Go.
Install the pinned toolchain and run the local gate with [mise](https://mise.jdx.dev/):

```sh
mise install
mise run check
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for prerequisites, focused test commands, and contribution guidance.

## Help and feedback

Report reproducible problems and propose improvements through [GitHub Issues](https://github.com/jozala/omnigrex/issues).
For an installation-specific failure, begin with the appropriate [repository troubleshooting](docs/repository-owners/troubleshooting.md) or [operator troubleshooting](docs/operators/troubleshooting.md) guide.
Include the revision, relevant configuration names, and sanitized diagnostics; keep credentials and private repository content out of public reports.

## License

Omnigrex is available under the [MIT License](LICENSE).

# Contributing to Omnigrex

Start with a [GitHub Issue](https://github.com/jozala/omnigrex/issues) describing the problem, desired behavior, and how to verify it.
Keep changes focused on that outcome and explain the reason for the change in the Pull Request.

## Set up

Clone the repository, install [mise](https://mise.jdx.dev/), and install Docker Engine with Compose v2 or Docker Desktop.
Docker is needed for the Compose configuration check and integration tests; the normal local gate does not start a deployment.

```sh
mise install
mise run check
```

The gate checks Go formatting, static analysis, dead code, race-enabled unit tests, Compose configuration, and local documentation links.
See [development](docs/contributors/development.md) for source-built images and local stacks, and [testing](docs/contributors/testing.md) for focused and backend-specific gates.

## Understand the project

- Read [AGENTS.md](AGENTS.md) for repository working conventions.
- Use [CONTEXT.md](CONTEXT.md) terminology.
- Start with the [architecture overview](docs/contributors/architecture.md), then read applicable [decisions](docs/decisions/README.md).
- Put implementation plans in `docs/plans/` and consequential choices in the decision log.
- Keep the accepted `docs/plans/first-iteration.md` specification intact.

Write behavior-focused tests for implementation changes and run the narrowest applicable tests plus `mise run check`.
Run Docker-backed integration tests when their behavior is affected.
The [testing guide](docs/contributors/testing.md) explains fixtures, backend requirements, and opt-in live tests.

## Documentation

Document current supported behavior in the relevant audience guide or reference page.
Link to authoritative operator procedures instead of copying secret, deployment, or recovery commands.
Update navigation and incoming links when moving a page.
Keep historical plans and evidence labeled as such; an accepted decision is not evidence that delivery is complete.

Use descriptive task titles, explain prerequisites and success conditions, and introduce terminology before relying on it.
Put each full sentence on one physical line when writing or substantially editing long Markdown files.
Use plain hyphens rather than em dashes.

```sh
mise run docs-check
```

The documentation check validates repository-local Markdown links and heading anchors without network access.
Operational shell examples also have focused executable checks; preserve those when moving procedures.

## Submit a change

Describe what changed, why, and the checks actually run.
Call out unavailable checks or remaining limitations explicitly.
Never include `.env`, private keys, provider credentials, private deployment metadata, compatibility artifacts, or backups in a commit or public report.

[Contributor guides](docs/contributors/README.md) · [Documentation hub](docs/README.md)

# Omnigrex

Omnigrex is a platform for coordinating autonomous software-development agents while keeping GitHub as the human-facing source of truth.

The project is currently defining and implementing its first dogfooding iteration:

```text
Human -> GitHub Issue -> Developer -> Pull Request -> Reviewer -> Human
```

## Documentation

- [Project concept](./project-concept.md)
- [First-iteration specification](./docs/first-iteration.md)
- [Implementation plan](./docs/implementation-plan.md)
- [Domain language](./CONTEXT.md)
- [Architecture decisions](./docs/adr/)

The implementation is written in Go and deployable with Docker Compose.

## Development

Install the pinned toolchain and run all local checks with [mise](https://mise.jdx.dev/):

```sh
mise install
mise run check
```

Build and start the current service locally with Docker Compose:

```sh
docker compose up --build
```

The liveness and readiness endpoints are available at `http://localhost:8080/healthz` and `http://localhost:8080/readyz` by default.
Copy `.env.example` to `.env` only when local overrides are needed.

Build the pinned OpenCode Runtime Process image and run its Docker-backed ACP compatibility tests with:

```sh
mise run test-integration
```

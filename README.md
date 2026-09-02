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

Docker Compose requires file-backed secrets and numeric access to the Docker socket.
Create local placeholders, then replace the GitHub and provider credential files before enabling those integrations:

```sh
cp .env.example .env
install -m 0700 -d secrets
openssl rand -base64 32 > secrets/database-password
: > secrets/github-developer-private-key.pem
: > secrets/github-reviewer-private-key.pem
openssl rand -hex 32 > secrets/github-webhook-secret
: > secrets/developer-provider-credentials.json
: > secrets/reviewer-provider-credentials.json
chmod 0640 secrets/*
```

Use `OMNIGREX_DOCKER_GID=0` from `.env.example` with Docker Desktop.
On Linux, replace it with the output of `stat -c '%g' /var/run/docker.sock`.
Create a dedicated host group for Omnigrex secret access, change `secrets/*` to that group, and set `OMNIGREX_SECRET_GID` in `.env` to its numeric GID.
The secret GID must differ from `OMNIGREX_DOCKER_GID`, and no unrelated host user should belong to the secret group.

Build the images and start PostgreSQL, the one-shot OpenCode validator, and the orchestrator:

```sh
docker compose up --build --wait
```

The liveness and dependency-aware readiness endpoints are available at `http://localhost:8080/healthz` and `http://localhost:8080/readyz` by default.
PostgreSQL and Runtime Process state use named volumes and survive container recreation; `docker compose down --volumes` removes them.

Build the pinned OpenCode Runtime Process image and run its Docker-backed ACP compatibility tests with:

```sh
mise run test-integration
```

The full-stack test deliberately recreates the stable local networks and volumes, so run it only when those resources can be removed:

```sh
mise run test-compose
```

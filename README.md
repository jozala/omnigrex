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

Docker Compose requires file-backed secrets, two GitHub Apps, and numeric access to the Docker socket.
Create the local secret directory and generated secrets:

```sh
cp .env.example .env
install -m 0700 -d secrets
openssl rand -base64 32 > secrets/database-password
openssl rand -hex 32 > secrets/github-webhook-secret
cp /secure/path/developer-opencode-auth.json secrets/developer-provider-credentials.json
cp /secure/path/reviewer-opencode-auth.json secrets/reviewer-provider-credentials.json
chmod 0640 secrets/*
```

Each provider credential file must contain the nonempty OpenCode authentication JSON object for that Role; an empty file or `{}` is rejected.

Place the downloaded Developer/Orchestrator and Reviewer GitHub App RSA private keys at `secrets/github-developer-private-key.pem` and `secrets/github-reviewer-private-key.pem` with mode `0640`.
Set both numeric App IDs in `.env`.
The Developer App requires exactly metadata and Checks read access, contents/Issues/Pull Requests/Workflows write access, and subscriptions to Issue, Pull Request, and Pull Request Review events.
The Reviewer App requires exactly metadata and contents read access plus Pull Request write access, no event subscriptions, and a disabled webhook.

Use `OMNIGREX_DOCKER_GID=0` from `.env.example` with Docker Desktop.
On Linux, replace it with the output of `stat -c '%g' /var/run/docker.sock`.
Create a dedicated host group for Omnigrex secret access, change `secrets/*` to that group, and set `OMNIGREX_SECRET_GID` in `.env` to its numeric GID.
The secret GID must differ from `OMNIGREX_DOCKER_GID`, and no unrelated host user should belong to the secret group.

Build the images and start PostgreSQL, the one-shot OpenCode validator, and the orchestrator:

```sh
docker compose up --build --wait
```

`OMNIGREX_OPENCODE_ACP_V1_IMAGE` must name an image in a registry by digest, not a mutable local tag.
Build the image, push it to a registry that retains the digest, and record the pushed repository digest:

```sh
mise run agent-image
docker tag omnigrex/opencode:1.18.29 registry.example/omnigrex/opencode:1.18.29
docker push registry.example/omnigrex/opencode:1.18.29
docker image inspect registry.example/omnigrex/opencode:1.18.29 --format '{{index .RepoDigests 0}}'
```

Set the resulting `registry.example/omnigrex/opencode@sha256:...` value in `.env` before starting the stack.

The liveness and dependency-aware readiness endpoints are available at `http://localhost:8080/healthz` and `http://localhost:8080/readyz` by default.
Configure the Developer App webhook through operator-provided HTTPS ingress at `/webhooks/github` using the value in `secrets/github-webhook-secret`.
PostgreSQL and Runtime Process state use named volumes and survive container recreation; `docker compose down --volumes` removes them.

## Preflight Diagnostics

Run the preflight against each repository before adding `omnigrex:run` to an Issue:

```sh
docker compose exec orchestrator /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

The command runs every independent check and prints one `PASS` or `FAIL` line for each prerequisite.
It exits with status `0` when all checks pass, `1` when a prerequisite fails, and `2` for invalid command arguments.

The checks cover configuration, secret formats, PostgreSQL connectivity and migration history, Docker API compatibility and required resources, exact Runtime Profile image availability, GitHub App identities, webhook settings and policies, repository installations, immutable Agent Profiles, effective Role permissions, ACP initialization, active OpenCode-to-MCP protocol compatibility, and configured MCP endpoint authentication.
The PostgreSQL connection is read-only and does not apply migrations.
Docker resource inspection does not write to persistent volumes.
The ACP check creates one isolated temporary container whose writable paths are tmpfs, initializes an authenticated diagnostic MCP connection through the agent network, and removes the container before returning.
GitHub installation checks create short-lived installation tokens but do not modify repository content or create a Workflow.
The MCP endpoint check expects a running orchestrator and confirms that an unauthenticated request is rejected with HTTP 401.

The command never prints private keys, webhook secrets, provider credentials, App JWTs, or installation tokens.

Build the pinned OpenCode Runtime Process image and run its Docker-backed ACP compatibility tests with:

```sh
mise run test-integration
```

The full-stack test deliberately recreates the stable local networks and volumes, so run it only when those resources can be removed:

```sh
mise run test-compose
```

The live GitHub gate uses a pre-existing Developer App-authored Pull Request and submits both Reviewer App outcomes.
Set the `OMNIGREX_LIVE_GITHUB_*` variables listed by the skipped test, then run:

```sh
mise run test-live-github
```

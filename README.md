# Omnigrex

Omnigrex is a platform for coordinating autonomous software-development agents while keeping GitHub as the human-facing source of truth.

The project is currently defining and implementing its first dogfooding iteration:

```text
Human -> GitHub Issue -> Developer -> Pull Request -> Reviewer -> Human
```

## Documentation

- [Project concept](./project-concept.md)
- [First-iteration specification](./docs/plans/first-iteration.md)
- [Implementation plan](./docs/implementation-plan.md)
- [Operator guide](./docs/operator-guide.md)
- [Dogfooding record](./docs/dogfooding.md)
- [Domain language](./CONTEXT.md)
- [Architecture decisions](./docs/adr/)
- [Project decision log](./docs/decisions/README.md)
- [Microsandbox feasibility experiment](./experiments/microsandbox/README.md)
- [Pi RPC feasibility experiment](./experiments/pi/README.md)
- [Pi and pluggable Execution Backend delivery plan](./docs/plans/2026-10-05-pi-and-pluggable-execution-backends.md)
- [Clean supported release baseline](./docs/plans/2026-10-16-clean-runtime-baseline.md)
- [Execution and agent boundary contracts](./docs/plans/2026-10-07-runtime-boundary-contracts.md)
- [Location-independent workspace evidence](./docs/plans/2026-10-08-location-independent-workspaces.md)
- [Retained-state bindings and recovery evidence](./docs/plans/2026-10-08-retained-state-bindings.md)
- [Repository environment contract and tooling evidence](./docs/plans/2026-10-08-repository-environments.md)
- [Microsandbox backend implementation and qualification](./docs/plans/2026-10-09-microsandbox-backend.md)
- [Pi protocol adapter and both-backend qualification](./docs/plans/2026-10-09-pi-protocol-adapter.md)
- [Pi configuration, authenticated MCP, and instruction qualification](./docs/plans/2026-10-09-pi-configuration-and-mcp.md)
- [Durable Pi Agent Sessions and History Replay](./docs/plans/2026-10-09-durable-pi-sessions.md)
- [Complete Pi Agent Turns and both-backend Workflow qualification](./docs/plans/2026-10-09-complete-pi-agent-turns.md)
- [Pi failure recovery and boundary independence](./docs/plans/2026-10-10-pi-failure-recovery.md)
- [Pi upgrades and state recovery](./docs/plans/2026-10-10-pi-upgrades-and-state-recovery.md)
- [Pi operational integration and backend CI](./docs/plans/2026-10-11-pi-operational-integration.md)
- [Pi deployment documentation evidence](./docs/plans/2026-10-11-pi-operator-documentation-and-cutover.md)
- [Pi-only source and final backend qualification](./docs/plans/2026-10-13-pi-only-final-qualification.md)

The implementation is written in Go and deployable with Docker Compose.

## Deployment

Deploy without cloning the repository by using the published GHCR images and pinned deployment files.
Follow the authoritative procedure in the [operator guide](./docs/operator-guide.md), including digest lookup, initial deployment, upgrades, retained Runtime Profile images, and restore prerequisites.
The [Pi deployment bundles](./docs/operator-guide.md#pi-operational-deployment) support Docker and Linux/amd64 Microsandbox with KVM using image-only deployment files.
Docker supports trusted-repository execution; Microsandbox additionally supports hardware isolation and guest Docker, Compose, and Testcontainers.
Select one backend per installation, without fallback or cross-backend state migration.
The [supported baseline](./docs/decisions/0024-establish-a-clean-supported-release-baseline.md) uses `pi-rpc/v2` and a fresh database/storage installation.
Deploy one commit whose matching images have passed publication and exact-digest smoke on both backends.
The [dogfooding record](./docs/dogfooding.md#current-baseline-validation) tracks live validation of that baseline after publication and operator deployment.

## Development

Install the pinned toolchain and run all local checks with [mise](https://mise.jdx.dev/):

```sh
mise install
mise run check
```

Build Pi with `mise run pi-image` and the Docker control plane with `mise run image`, or build `Dockerfile.microsandbox` on a Linux/amd64 builder.
Use the [Pi deployment procedure](./docs/operator-guide.md#pi-operational-deployment) with actual registry digests for a configured local stack.
The root `compose.yaml` and `.env.example` provide the Pi-only Docker development stack, with a source-built orchestrator and an exact registry-pinned Pi image.
Set `OMNIGREX_PI_IMAGE` and its matching platform, provision that image, and run `docker compose up --build --wait` after configuring the required secrets.
For a locally built Pi environment, publish it to a development registry and select its resolved digest using the image procedure in the operator guide.
Secret placement, GitHub Apps, ingress, image provisioning, and recovery procedures are authoritative in the [operator guide](./docs/operator-guide.md).

## Preflight Diagnostics

After selecting the deployment's Compose file set as documented in the operator guide, run preflight against each repository before adding `omnigrex:run` to an Issue:

```sh
docker compose exec orchestrator /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

The command runs every independent check and prints one `PASS` or `FAIL` line for each prerequisite.
It exits with status `0` when all checks pass, `1` when a prerequisite fails, and `2` for invalid command arguments.

The checks cover configuration and secrets, PostgreSQL, selected-backend resources, exact configured and protected images/state, GitHub Apps and installations, repository environments, Agent Profiles, effective Role permissions, and authenticated runtime-to-MCP compatibility.
The PostgreSQL connection is read-only and does not apply migrations.
The Pi runtime check creates an isolated Execution Environment with temporary Session storage, verifies model/thinking and authenticated diagnostic MCP discovery without submitting a model prompt, then removes it.
GitHub installation checks create short-lived installation tokens but do not modify repository content or create a Workflow.
The MCP endpoint check expects a running orchestrator and confirms that an unauthenticated request is rejected with HTTP 401.

The command never prints private keys, webhook secrets, provider credentials, App JWTs, or installation tokens.

Run Docker-backed integration tests with:

```sh
mise run test-integration
```

The full-stack gate uses disposable Pi networks and volumes to check authenticated durable webhooks, persistent storage, network isolation, dependency readiness, and operational diagnostics:

```sh
mise run test-compose
```

The SDK-backed Microsandbox adapter has a separate Linux/amd64 KVM qualification gate:

```sh
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-microsandbox-integration
```

Use an approved Linux Docker host through `DOCKER_HOST` when developing on macOS.
See the [operator guide](./docs/operator-guide.md#microsandbox-execution-backend) for native build, backend selection, storage, and resource guarantees.

Run the Pi protocol adapter gates with:

```sh
mise run test-pi-integration
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-integration
```

Both use a disposable pinned Pi image and a controlled provider with fixture credentials.
See the [operator guide](./docs/operator-guide.md#pi-protocol-diagnostic) for the standalone diagnostic command.

The complete backend gates, including shared conformance and Pi lifecycle qualification, are:

```sh
mise run test-docker-integration
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-microsandbox
```

Image-only deployment smoke uses `mise run test-pi-deployment` or `mise run test-pi-microsandbox-deployment` on the Docker daemon's host, with isolated fixture secrets and resources.
See the [operator guide](./docs/operator-guide.md#diagnostics-and-backend-gates) for remote-host and published-image qualification.

The live GitHub gate checks the Developer App's repository-ID read and uses a pre-existing Developer App-authored Pull Request to submit both Reviewer App outcomes.
Set the `OMNIGREX_LIVE_GITHUB_*` variables listed by the skipped test, then run:

```sh
mise run test-live-github
```

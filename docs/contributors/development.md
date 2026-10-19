# Set up source development

[Contributor guide](README.md) · [Testing](testing.md)

## Install and check

Clone the repository and install mise, Git, and Docker with Compose v2.
The pinned Go toolchain and dead-code tool are declared in [mise.toml](../../mise.toml).

```sh
mise install
mise run check
```

The normal gate formats nothing automatically: it checks formatting, static analysis, dead code, race-enabled unit tests, Compose rendering, and documentation links.
Use `mise run format` to apply Go formatting when needed.
Compose rendering uses fixture values and does not require deployment secrets or start containers.

## Build the images

```sh
mise run pi-image
mise run image
```

For the SDK-enabled controller, build on Linux/amd64:

```sh
docker build --file Dockerfile.microsandbox --target runtime --tag omnigrex/orchestrator-microsandbox:dev .
```

The native image packages pinned runtime/firmware/SDK dependencies and the source-pinned guest agent.
The ordinary Dockerfile builds the static Docker backend.
Selecting Microsandbox in a binary without SDK support fails explicitly.
See [backend constraints](../operators/backends.md).

## Run a local stack

The root [compose.yaml](../../compose.yaml) and [.env.example](../../.env.example) describe the Docker development stack.
It source-builds the orchestrator and uses an exact registry-pinned Pi image.
Complete the authoritative [App](../operators/github-apps.md), [secret](../operators/secrets.md), [ingress](../operators/installation.md#https-ingress), and [repository setup](../repository-owners/connect-a-repository.md) procedures first.

Copy `.env.example` to a private `.env`, configure required values, and set a real `OMNIGREX_PI_IMAGE` digest with matching `OMNIGREX_PI_PLATFORM`.
For a locally built Pi image, publish it to a development registry and resolve its digest using the [image procedure](../operators/installation.md#3-resolve-and-provision-images).
Provision the exact image before startup; Docker-backed arm64 development is supported with a matching local image/platform.

From the checkout, select only the root development Compose file, keeping any explicit overrides in the same selection:

```sh
export COMPOSE_FILE=compose.yaml
docker compose up --build --wait
docker compose exec orchestrator /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

Do not reuse a downloaded no-clone bundle's Compose selection by accident.
Default named resources are fixed; parallel setups need complete storage/network/namespace overrides as described in [installation](../operators/installation.md#2-stage-deployment-files).
Require all-pass preflight and real webhook verification before a test Work Item.

## Develop through Omnigrex

Agents working on this repository need its system tools and Docker-backed tests inside the execution environment.
Use the [Omnigrex development image](development-image.md) with Microsandbox rather than expecting the Docker backend to supply a host daemon.
The agent environment has a private guest daemon and no host Docker socket.

Next: [Choose tests](testing.md).

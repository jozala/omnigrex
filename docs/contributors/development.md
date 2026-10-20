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

### Docker controller package policy

The controller installs `bash`, `ca-certificates`, `curl`, `git`, `jq`, `gcc`, `musl-dev`, `openssh-client-default`, and `ripgrep` with `apk add --no-cache` and normal signature verification.
Each build records its actual installed closure, including base and transitive packages, as sorted `apk info -v` output at `/usr/share/omnigrex/apk-packages.txt`.
Inspect the inventory bound to an exact image digest without relying on mutable tags.

```sh
docker run --rm --entrypoint /bin/cat IMAGE@sha256:DIGEST /usr/share/omnigrex/apk-packages.txt
```

Verify a built controller image against required tools, compiler and header function, Git HTTPS support, CA certificates, non-root execution, and inventory agreement.

```sh
sh scripts/check-orchestrator-image.sh omnigrex/orchestrator:dev-amd64
sh scripts/check-orchestrator-image.sh omnigrex/orchestrator:dev-arm64
```

Refresh installed OS packages intentionally so the package-installation layer is genuinely uncached on each checked architecture.

```sh
docker buildx build --no-cache-filter runtime --platform linux/amd64 --load --tag omnigrex/orchestrator:dev-amd64 .
docker buildx build --no-cache-filter runtime --platform linux/arm64 --load --tag omnigrex/orchestrator:dev-arm64 .
```

Docker build-layer caching and APK `--no-cache` are distinct mechanisms.
The former controls whether Docker reuses the installation layer, while the latter controls only whether APK keeps a local package cache inside that layer.
Retain released images for recovery because rebuilding a source commit is not a substitute for preserving its qualified image.
See [controller package decision](../decisions/0033-resolve-controller-packages-from-signed-alpine-repositories.md).

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

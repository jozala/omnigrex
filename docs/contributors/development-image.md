# Build and promote the Omnigrex development image

[Contributor guide](README.md) · [Repository environments](../repository-owners/development-environments.md)

The image at `agent/environments/omnigrex/Dockerfile` supports agents developing Omnigrex itself.
It extends Pi with checksum-locked Docker Engine 29.8.1, Compose 5.6.0, Buildx 0.38.0, and Debian Bookworm/amd64 iptables/jq dependency closures.
The golden image already provides GCC and Make.
The derived environment preserves Pi resources, entrypoint, workspace, UID/GID 10001, and its OCI source label.

## Prerequisites

Use Microsandbox on Linux/amd64 KVM with `hardware-isolation`, `nested-containers`, and `tool-installation` capabilities.
Use the matching release's native controller with its patched guest agent; changing only the repository image cannot repair memory-controller delegation in an older controller.
New Participants have a 512-PID aggregate workload ceiling, while retained canonical 128-PID contracts keep their budgets and identities.
Older controllers cannot read new 512-PID contracts; preserve compatible recovery controllers.
See [decisions 0028](../decisions/0028-keep-a-domain-parent-for-nested-containers.md) and [0029](../decisions/0029-preserve-historical-pid-budgets-and-size-new-participants.md).

## Build and exercise locally

```sh
mise run development-image
mise run test-development-image-cache
mise run test-development-image-unit
```

The build first creates the current checkout's Linux/amd64 `omnigrex/development-golden:dev`, so bootstrap/storage paths match even before publication.
Release CI instead uses the same release's exact published golden digest.
For an externally qualified base, build with `--platform linux/amd64 --target development --file agent/environments/omnigrex/Dockerfile --build-arg OMNIGREX_GOLDEN_IMAGE=<exact-golden-reference>`.
The base must provide supported `pi-rpc/v2` storage under `/home/omnigrex/.local/share/mise` and `/home/omnigrex/.local/share/omnigrex-tool-data`.
Pre-baseline golden images are unsupported.

Only the `development` target is published.
Cache/repository qualification targets add disposable fixtures excluded from the production image.
Before publication, the source-built image must pass the unit suite with its actual system tools, including jq-based deployment fixtures.

## Module source and scratch behavior

Module archives/metadata live at `/opt/omnigrex/go-proxy`, outside attached workspace/state/tool data.
The image includes project dependencies and those downloaded while installing Go tools pinned in mise.
Working-revision `scripts/development-environment.sh` prepends the file proxy only when present, retaining an existing upstream proxy or standard public/direct fallback.
It is idempotent and leaves ordinary machines unchanged when the directory is absent.
Go fills writable Participant module caches on demand, without a bootstrap-copy mechanism.
Keep `GOPATH` and `GOCACHE` approved in [operator policy](../operators/agent-policy.md#approve-tool-path-variables) and retain `.omnigrex/turn-configuration.yaml`.

New versions fall back upstream and cache for later Turns; working-revision module files remain authoritative.
Checksum verification remains enabled and can require network access for missing records.
The image-local source does not persist the Go distribution, startup installation data, compiled artifacts, or modules downloaded in separate Docker builds.
See [decision 0026](../decisions/0026-use-an-image-local-go-module-source.md).

`scripts/go-test.sh` selects native guest `/var/tmp` for Go test/compiler scratch in this image.
Shared tool-data mounts virtualize permission changes and cannot faithfully test restrictive modes, while Runtime Profile `/tmp` is only 64 MiB.
The override is command-local and preserves persistent caches and parent allocated paths.
Go cleans its private temporary directories, and environment removal discards abnormal leftovers.
This qualifies native scratch, not permission fidelity of retained mounts; see [decision 0027](../decisions/0027-use-native-scratch-for-development-go-tests.md).

## Qualify an exact published digest

CI publishes `ghcr.io/jozala/omnigrex/development:sha-<commit>` and records the exact digest/golden base.
The `development-smoke-microsandbox` job runs actual checkout and Docker integration checks across two fresh environments sharing Participant caches and Pi Session.
It tests nested Compose networking/volumes/cleanup, concurrent orphan/sentinel containers, historical/current PID persistence, nested background termination, and real Pi diagnostic MCP initialization.
Only a successful job emits the digest-pinned `development-environment` artifact for repository promotion.

To repeat qualification on a Linux/amd64 Docker host with KVM:

```sh
OMNIGREX_DEVELOPMENT_IMAGE=ghcr.io/jozala/omnigrex/development@sha256:<qualified-digest> \
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> \
mise run test-development-image-microsandbox
```

The fixture allocates a 4 GiB VM, 40 GiB root disk, and 6 GiB wrapper with fixture-only diagnostic credentials.
It does not provide nested KVM; native Microsandbox qualification remains a separate CI-host gate.

## Promote to the repository

Publish and qualify before promoting the artifact to `.omnigrex/environment.yaml` on the default branch.
The checked-in `.omnigrex/environment.yaml.example` has no usable digest and is never loaded as configuration.
Ask the operator to provision the exact image, retain readable registry artifacts for flat materialization, and run repository doctor before work.
Later promotions update the digest explicitly; retained Participants keep their original images/caches.
The environment document does not size VM memory or disk: set those deployment limits for the actual build/test workload.
See the [delivery plan](../plans/2026-10-16-development-image.md) for recorded implementation evidence.

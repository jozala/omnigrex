# Choose the right verification gate

[Contributor guide](README.md) · [Runtime qualification](runtime-qualification.md)

## Normal local work

```sh
mise run check
```

This runs formatting checks, `go vet`, dead-code analysis, race-enabled unit tests, Compose validation, and repository-local documentation links.
Run the narrowest relevant tests during implementation, then this gate for code changes.
Use `mise run docs-check` for a documentation-only link/anchor pass.
See [mise.toml](../../mise.toml) for the exact executable commands.

CI restores the check job's Go module/build cache and separate BuildKit layer caches for the check, Microsandbox, publication, and development-image smoke jobs.
The image-build scripts use the BuildKit cache only when `OMNIGREX_BUILDKIT_CACHE_DIR` is set; local commands keep their usual Docker build behavior.
Each cached CI image is loaded into Docker for its existing qualification consumer, and tests run with fresh Go results even when compilation is cached.
The check job also uses a disposable local registry to expose its source-built Pi and development images to the isolated BuildKit container for chained image builds.
Run `sh scripts/test-ci-buildkit.sh` for a Docker-backed parent/child build and cache-pruning regression; it creates disposable local builder and registry resources when they are not already present.
The Microsandbox gate runs the patched guest-agent workload tests through `test-agentd` with a new invocation nonce, so a warm image-build cache cannot skip them.
The cache is disposable, so a miss can lengthen a build but cannot change the required cold/offline module checks or exact-digest release gates.
See [decision 0035](../decisions/0035-persist-ci-build-layers-without-runtime-state.md) for cache scope and trust boundaries.

## Docker-backed integration

```sh
mise run test-integration
```

This builds the Pi image and checks doctor, Docker runtime, PostgreSQL/store/migrations, and deployment behavior with Docker-backed fixtures.
Use Docker Engine or Docker Desktop with enough capacity and network access for fixture images.

The executable documentation examples can be checked independently:

```sh
mise exec -- go test ./internal/deployment -run TestPiOperatorRestoreRequiresConfirmedNativeAbsence -count=1
mise exec -- go test -tags=integration ./internal/deployment -run TestPiOperatorExamplesPreserveComposeSelection -count=1
```

The restore test uses stub commands and temporary data.
The Compose example test renders the documented selections without deploying or changing protected state.
The Docker-backed gate exercises the controller image check described in [source builds](development.md#docker-controller-package-policy).

## Complete backend qualification

```sh
mise run test-docker-integration
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-microsandbox
```

The Microsandbox gate requires a Linux/amd64 Docker host with usable KVM.
On that host, obtain the group with `stat -c '%g' /dev/kvm`.
From macOS, use an existing authorized remote `DOCKER_HOST=ssh://USER@HOST` and that host's numeric KVM group.
Most source gates transfer fixtures over stdin rather than mounting client-side files.
Image-only deployment smoke must run on the daemon host because Compose secrets are host files.

| Focus | Docker task | Microsandbox task |
| --- | --- | --- |
| Runtime/storage/resources | `test-integration` | `test-microsandbox-integration` |
| Pi protocol/configuration/Sessions | `test-pi-integration` | `test-pi-microsandbox-integration` |
| Complete controlled Workflow | `test-pi-workflow` | `test-pi-microsandbox-workflow` |
| Failure recovery | `test-pi-recovery` | `test-pi-microsandbox-recovery` |
| Upgrade and retained state | `test-pi-upgrade` | `test-pi-microsandbox-upgrade` |
| Image-only deployment | `test-pi-deployment` | `test-pi-microsandbox-deployment` |

`mise run test-compose` delegates to disposable Pi deployment smoke.
Its fixtures use isolated names and remove their own resources; inspect the task/script before running a full-stack gate against a shared host.
The production Compose files retain fixed defaults and are not isolated merely by changing the project name.

## Live external gates

The GitHub gate performs a Developer App repository-ID read and submits both Reviewer outcomes against a pre-existing Developer App-authored Pull Request.
It can mutate live review state.
Supply the `OMNIGREX_LIVE_GITHUB_*` values listed by the skipped test, using a designated fixture repository and Pull Request, then run:

```sh
mise run test-live-github
```

For a paid provider prompt through the production runner, see [live-provider smoke](runtime-qualification.md#live-provider-smoke).
Live gates are opt-in and separate from deterministic controlled-provider CI.
Never substitute real credentials into ordinary fixture gates.

## Release evidence

CI gates publication on Docker and required Linux/amd64 KVM qualification, then repeats image-only smoke against actual published registry digests.
The development image has additional exact-digest native qualification before repository promotion.
Passing unit tests or fixture aliases alone does not establish a deployable release.
See [runtime qualification](runtime-qualification.md) and [release selection](../operators/installation.md#1-select-a-qualified-commit).

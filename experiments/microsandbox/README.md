# Microsandbox development-environment experiment

This is the reproducible Step 01 probe for the [Pi and execution-backend plan](../../docs/plans/2026-10-05-pi-and-pluggable-execution-backends.md).
It is an opt-in integration test, not a production execution backend.
See the [evidence and remaining qualification boundaries](../../docs/experiments/microsandbox.md).

The later production adapter uses the pinned Go SDK and has its own [Step 08 qualification](../../docs/plans/2026-10-09-microsandbox-backend.md).
Run `mise run test-microsandbox-integration` for that adapter's gate; this feasibility entry point continues to describe the historical CLI topology proof.

## Requirements

- The repository's pinned Go toolchain and a Docker CLI on the machine running the test.
- A Linux/amd64 Docker daemon whose host has working `/dev/kvm`.
- The numeric group owning `/dev/kvm` on that Docker host.
- Capacity for a wrapper limited to two CPUs, 6 GiB memory, and 1,024 PIDs, containing a two-vCPU, 4 GiB development microVM with a 20 GiB sparse root disk.
- Public registry, Alpine package, GitHub, Go download, and Go module access.

Docker Desktop without a usable KVM device cannot run the microVM probe.
The local Go test process can instead target an operator-approved Linux Docker daemon through an existing SSH connection.

## Run against a local Linux Docker daemon

```sh
export OMNIGREX_MICROSANDBOX_KVM_GID="$(stat -c '%g' /dev/kvm)"
mise run test-microsandbox-feasibility
```

## Run against an approved remote Linux Docker daemon

Replace the example SSH destination with an existing authenticated connection.
Do not commit its address, credentials, or captured deployment metadata.

```sh
export DOCKER_HOST=ssh://USER@LINUX_HOST
export OMNIGREX_MICROSANDBOX_KVM_GID="$(ssh USER@LINUX_HOST stat -c '%g' /dev/kvm)"
mise run test-microsandbox-feasibility
```

The remote host does not need a Go toolchain, mise, a repository clone, or a host installation of Microsandbox.
The test uses the Docker CLI's configured connection and transfers fixtures through `docker cp`, not client-side bind mounts.
Selecting a remote Docker daemon for this experiment does not implement a remote Omnigrex backend.

## What runs

1. Pull the digest-pinned Microsandbox 0.7.6 wrapper image and run its KVM diagnostics as UID/GID `10001:10001` with the KVM device group.
2. Create two successive non-root Alpine microVMs and verify that an explicit retained mount survives while disposable guest files do not.
3. Copy the embedded fixtures and a source archive of the repository's committed `HEAD` into the wrapper.
4. Boot a Docker 29.7.2-dind microVM on a flat ext4 root disk and start its own Unix-socket Docker daemon.
5. Verify Compose service health and service-to-service HTTP networking.
6. Verify a Testcontainers-created service through its published HTTP port, including the Ryuk resource reaper.
7. Build the existing OpenCode image from the archived source and run `TestReadinessProbeExercisesRealDockerResources` inside the microVM.
8. Remove the microVMs and their wrapper.

The development guest runs as root to start its private Docker daemon.
The outer Microsandbox process remains non-root and receives neither the host Docker socket nor host repository/secret mounts.
This demonstrates the execution topology, not the final Agent Profile privilege policy.

The source archive deliberately excludes uncommitted changes, `.git`, and untracked local secrets.
The test logs the exact source revision used.
The new experiment fixtures are embedded from the current checkout, including uncommitted fixture changes.

Testcontainers has its own dependency lock in `testdata/testcontainers/fixture.mod` and `fixture.sum`, leaving production dependencies unchanged.
Those filenames also prevent Go's embed machinery from excluding the fixture as a nested module.
The guest materializes the standard module filenames before testing.

## Pinning and evidence

The Microsandbox, Docker-dind, Alpine, BusyBox HTTP fixture, and Ryuk images use OCI digests.
The Go 1.27.0 archive is checksum-verified.
Testcontainers is pinned to 0.44.0 with its transitive checksums.
Microsandbox's Go SDK candidate is the matching `v0.7.6`; this CLI experiment does not yet qualify the SDK transport.

Additional guest packages are installed from the image's Alpine repositories, and their resolved versions are printed.
That installation intentionally exercises in-environment tooling; it is not a hermetic release-image build.

The probe reports whether the guest exposes `/dev/kvm`, but does not claim that another nested microVM can boot.
The plan assigns Microsandbox-specific integration tests to the outer KVM CI host.

## Cleanup and failure behavior

Every execution uses a random wrapper name with the label `io.omnigrex.experiment=microsandbox-feasibility`.
Normal completion and test failures force-remove only that wrapper and its anonymous volumes.
The wrapper has a 45-minute lifetime and automatic removal as a fallback if the local test process disappears.
Guest execution also has explicit deadlines.
No Docker prune operation is used, and no host port is published.
The pulled Microsandbox image remains in the host's image cache.

If the Docker connection fails during cleanup, the test reports the exact wrapper name for targeted operator cleanup.
Deleting the wrapper terminates its VMM processes and removes the experiment's writable filesystems.

## Local fixture checks

These checks do not require KVM:

```sh
go test -race -tags=integration ./experiments/microsandbox -run 'Test(FixtureArchive|CommittedSourceArchive)'
docker compose -f experiments/microsandbox/testdata/compose.yaml config --quiet
sh -n experiments/microsandbox/testdata/guest.sh
```

The regular `mise run check` remains the repository gate.
The experiment is outside the normal Docker integration package list and runs only through its explicit entry point.

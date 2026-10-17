# Omnigrex development environment image

## Scope

Implement [decision 0026](../decisions/0026-use-an-image-local-go-module-source.md) using the supported `pi-rpc/v2` golden-image contract and repository environment schema.
The target is Microsandbox on Linux/amd64 with a private guest Docker daemon, Compose, Buildx, and the golden image's existing C compiler and build tools.
The development Dockerfile owns the OCI source label and adds checksum-locked container tooling and the Debian networking and jq dependency closures without replacing Pi resources or startup behavior.
The native control-plane image carries the narrowly patched guest-agent placement described in [decision 0028](../decisions/0028-keep-a-domain-parent-for-nested-containers.md), preserving one aggregate PID budget while enabling nested container memory limits.
New Participants receive the 512-PID budget in [decision 0029](../decisions/0029-preserve-historical-pid-budgets-and-size-new-participants.md), with earlier 128-PID canonical contracts within the supported baseline retained exactly.

## Module source

Build the module archive seed from the checked-in `go.mod`, `go.sum`, and pinned Go-based development tool.
Store it outside runtime attachments at `/opt/omnigrex/go-proxy`.
Working-revision mise configuration selects this file source only when it exists, preserves an existing upstream proxy, and does not change normal developer environments.
Go fills the existing per-Participant writable module cache lazily; existing build and module cache lifetimes remain unchanged.
The ordinary Go test tasks select native guest scratch through a command-local wrapper as recorded in [decision 0027](../decisions/0027-use-native-scratch-for-development-go-tests.md), preserving permission-sensitive test coverage without changing runtime path allocation.

## Delivery and promotion

Pull-request CI builds against the source-built golden image and verifies the module source in a non-networked, read-only disposable container.
It also runs the ordinary repository unit suite with the development image's actual tools, without a host Docker socket or secrets, to catch missing test dependencies before publication.
Release CI publishes the development image using the newly published golden digest and records both identities.
After the ordinary published Pi deployment gates, a dedicated Linux/KVM job qualifies the exact development digest and emits an `environment.yaml` artifact.
The first publication precedes the repository change that promotes that real document to `.omnigrex/environment.yaml`.
The checked-in `.omnigrex/environment.yaml.example` is illustrative and is not active configuration.
Deployment and image provisioning procedures remain authoritative in the [operator guide](../operator-guide.md#omnigrex-development-environment).

## Verification

- Build the production and disposable cache-qualification image targets.
- Verify mise activation, project downloads with ordinary checksum validation and no external network, controlled fallback for a missing version, writable cache reuse, and Participant cache separation.
- Run `mise run check` and `mise run test-integration`.
- Compile the native integration test and run the exact-digest Microsandbox gate on a qualified Linux/amd64 KVM host.
- Qualify memory-limited containers before and after a nested build, an empty domain parent, aggregate PID ancestry, and subsequent SDK execs with the patched guest agent.
- In that gate, verify golden resources, non-root guest Docker, a real Compose lifecycle including readiness, service connectivity and resource cleanup, repository checks and Docker-backed tests, module cache reuse and Pi Session Continuation across fresh VMs, whole-environment cleanup with a nested workload, and authenticated Pi diagnostic readiness.

# Microsandbox feasibility evidence

## Scope

This record covers the initial Linux execution-environment proof from [Step 01 of the combined plan](../plans/2026-10-05-pi-and-pluggable-execution-backends.md).
The executable experiment and reproduction instructions live in [experiments/microsandbox](../../experiments/microsandbox/README.md).
Deployment and secret procedures remain in the [operator guide](../operator-guide.md).

## Observed on 2026-10-05

The experiment ran against an operator-provided Linux/amd64 Docker daemon with a usable KVM device.
The test client ran separately and used Docker over SSH.
No host configuration or KVM module policy was changed.

| Boundary | Evidence |
| --- | --- |
| Containerized non-root Microsandbox | Microsandbox 0.7.6 doctor passed with UID/GID 10001 and the host's KVM device group |
| Hardware virtualization | A real microVM booted with guest kernel 6.12.109 |
| Retained versus disposable data | An explicit mount retained its marker after the first microVM was removed; the fresh microVM had no prior disposable marker |
| Guest Docker daemon | Docker 29.7.2 started inside the microVM on a flat ext4 root disk |
| Guest resources | The nested engine reported two CPUs and approximately 3.94 GiB available memory for the configured 4 GiB guest |
| Compose | Compose 5.5.0 started a healthy HTTP service and a separate client reached it by service name |
| Testcontainers | Testcontainers-Go 0.44.0 started Ryuk and an HTTP fixture; the published-port request returned the expected body |
| Existing Omnigrex integration | The guest built OpenCode 1.18.29 and passed `TestReadinessProbeExercisesRealDockerResources` with the race detector |
| Resource cleanup | After the run, the host had no wrapper containers carrying the experiment label |

The first complete successful run took approximately four minutes.
Its archived Omnigrex source revision was `e66418a3f80da6f4e9cca6019fedbf26e9349fd6`.
The experiment records the revision on every run because it tests committed source rather than silently including local changes.

## Interpretation

The tested topology supports the central development-environment requirement: a microVM can run its own Docker daemon, Compose, Testcontainers, and an existing Omnigrex Docker integration test without receiving the host Docker socket.
The host can run the Microsandbox process as a non-root user with explicitly granted KVM access.
Guest-root access for the private Docker daemon does not require a privileged outer Docker container in this experiment.

The available Linux host is a candidate for subsequent manual qualification and a KVM-capable CI runner.
It has not been registered as a persistent CI runner; CI integration remains a later delivery step.

## Limits and next proof

- The CLI and kernel topology are exercised; the matching Go SDK `v0.7.6` is a candidate, not a qualified integration.
- Retained files survive guest replacement within one wrapper; this does not yet prove production backup/restore or wrapper/host replacement.
- The development guest is root; the eventual agent identity, tool installation privileges, and Docker access policy still need the environment contract.
- Only the named Omnigrex readiness integration test runs in the guest, not the entire integration suite or destructive Compose gate.
- The guest exposes `/dev/kvm`, but nested microVM execution has not been tested and is not a release requirement.
- The experiment uses public egress; authenticated MCP connectivity and its intended network policy are part of the following Pi feasibility work.
- Requested memory/CPU sizing is checked, but failure attribution, OOM behavior, ambiguous creation, and crash recovery still need backend conformance tests.
- No Pi provider requests, persistent Pi sessions, credential-handling guarantees, or production backend substitution have been proved yet.

The subsequent [Pi protocol, MCP, and session feasibility evidence](pi.md) builds on this execution environment.

# Execution backends

[Operator guide](README.md) · [Documentation hub](../README.md)

## Choose one backend

The backend overlay selects `OMNIGREX_EXECUTION_BACKEND=docker` or `microsandbox` for the entire installation.
There is no silent fallback, mixed-backend routing, or automatic retained-state conversion.
The ledger's backend ownership, contract version, and storage namespace are immutable.
Changing backend requires a fresh installation or a separately designed disposal/migration procedure.

| Guarantee or requirement | Docker | Microsandbox |
| --- | --- | --- |
| Host | Linux Docker Engine or Docker Desktop on macOS | Linux/amd64 with usable KVM |
| Execution isolation | Containers for trusted repositories | Hardware-isolated microVMs |
| Working-checkout mise installation | Supported | Supported |
| Docker, Compose, Testcontainers inside an Agent Turn | Unsupported required capability | Qualified private guest daemon |
| Host execution authority | Control-plane Docker socket | Control-plane KVM device |
| Whole-environment stop and exact-epoch cleanup | Required | Required, including nested workloads |
| Consistent captures and retained Session ownership | Required | Required |
| Resource evidence | Container limits and Docker exit/OOM observations | Guest limits; unknown crash/writeback evidence stays unknown |
| Provider-key mediation | Not implemented | Not implemented |

Both reject unsupported capabilities before repository execution.
Backend selection does not change Workflow, Role, MCP, or trusted publication policy.
Nested microVM execution is investigated, not a release gate.

## Microsandbox prerequisites

The native image pins Microsandbox and its Go SDK to 0.7.11.
It includes the pinned runtime, firmware, SDK library, CGO-enabled orchestrator, and trusted repository preparation tools.
The source-pinned guest-agent patch selected by `MSB_AGENTD_PATH` places workload processes under `/microsandbox-workload/agent`, leaving the parent empty for Docker memory-controller delegation, aggregate PID limits, and freezing.
See [decision 0028](../decisions/0028-keep-a-domain-parent-for-nested-containers.md) and [backend evidence](../plans/2026-10-09-microsandbox-backend.md).
Image-only hosts need no Rust or Go toolchain.
For the coordinated v0.7.11 catalog migration, qualification, rollout, and backup recovery requirements, see [Microsandbox upgrade](microsandbox-upgrade.md).

The control-plane process uses UID/GID 10001 and needs read/write KVM access through the device's numeric group.
Use a separate dedicated group for secret files.
The guest receives no host Docker socket; nested containers use its private daemon.
Guest-root code can still modify guest controls and access provider credentials supplied to that environment.
Allocate enough host capacity for admitted VMs and VMM overhead.

## Native configuration and storage

| Variable | Meaning and default |
| --- | --- |
| `OMNIGREX_MICROSANDBOX_EXECUTABLE` | Pinned executable, `/usr/local/bin/msb` |
| `OMNIGREX_MICROSANDBOX_FIRMWARE` | Firmware library, `/usr/local/lib/libkrunfw.so.5.6.1` |
| `OMNIGREX_MICROSANDBOX_HOME` | Inventory, image cache, disposable VM resources, and private dispatch journal; `/var/lib/omnigrex/msb` |
| `OMNIGREX_MICROSANDBOX_NAMESPACE` | Immutable installation namespace, `omnigrex` |
| `OMNIGREX_MICROSANDBOX_CPUS` | Virtual CPUs, 1-64; default 2 |
| `OMNIGREX_MICROSANDBOX_ROOT_DISK_MIB` | Root capacity, 1,024-1,048,576 MiB; default 20,480 |

Keep the native home path at most 40 bytes for the pinned Unix-socket budget.
It must be private to the control plane and disjoint from workspace, retained-state, and tool roots.
Retain Session data and protected Workspace Captures separately with matching ownership.
Never bind native home or entire storage roots into a guest.

## Provision exact images

Before accepting work, provision exact repository images into the selected cache using installed `msb image pull` or archive import commands.
The [installation procedure](installation.md#microsandbox-bundle) gives the Compose invocation.
Admission verifies actual manifest/config content and platform linkage; a digest-shaped imported alias is insufficient.
First-use verification of an index-pinned image or SDK-reserialized registry manifest requires bounded HTTPS access to its exact registry artifacts.
Warm verification reuses validated immutable documents from the persistent `omnigrex-registry-documents/v1` cache under the native home.
Repeated readiness checks make no registry requests for those documents, including after orchestrator restarts.
Ordinary execution and golden-resource diagnostics use cached layered artifacts without pulling.

Nested containers require flat ext4 materialization, which may fetch the declared registry digest during bounded preparation.
Guest workload pulls may also need registry access.
Keep those exact registry artifacts available.
Omnigrex does not build repository images or fall back to mutable tags.

Deleting the document cache is safe but requires registry access to rebuild verification evidence.
It contains reproducible evidence only and is not required Session backup data.
See [decision 0030](../decisions/0030-cache-immutable-registry-documents-for-offline-verification.md).

## Failure and recovery boundaries

CPU/memory allocation and workload PID limits are guest-scoped, not host VMM-thread limits.
Transport loss, cancellation, exit code 137, and unknown guest exits do not establish OOM or successful cleanup.
After an unflushed crash, required workspace evidence may remain unavailable and settlement preserves that uncertainty.
An unresponsive guest is force-terminated by exact identity under a finite budget, without claiming successful writeback or terminal capture.
The SDK's crashed state excludes writers and allows preparation-only cleanup; it supplies no missing OOM/writeback evidence.

Include the protected `omnigrex-operations` journal in the coordinated [backup](backup.md) and [restore](restore.md).
Exclude disposable VM disks, snapshots, and tmpfs credentials from normal retained-state archives.
Cross-backend archives are rejected, and the original host must stay fenced during compatible-host restore.
Dispatch journals support recovery cleanup but do not let a restarted controller adopt prior ephemeral registration inputs.

Next: [Installation](installation.md) or [backend qualification for contributors](../contributors/runtime-qualification.md).

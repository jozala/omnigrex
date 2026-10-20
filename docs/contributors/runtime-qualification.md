# Qualify runtime behavior and release images

[Contributor guide](README.md) · [Testing](testing.md)

Run qualification in disposable environments with fixture authority, not against protected deployment data.
Use the selected backend's [host and storage prerequisites](../operators/backends.md).
For remote native gates, use an existing authorized `DOCKER_HOST` connection and the daemon host's KVM group.

## Pi protocol diagnostic

The disposable Pi 1.0.4 diagnostic exercises the adapter with a controlled HTTP provider, no real credentials, no GitHub/business MCP authority, no Workflow, and temporary tmpfs Session storage.
Build the image and resolve exact local content identity:

```sh
mise run pi-diagnostic-image
docker image inspect omnigrex/pi-diagnostic:step09 --format '{{.Id}} {{.Architecture}}'
```

Use the returned ID and architecture with the source-built command:

```sh
mise exec -- go run ./cmd/omnigrex pi-diagnostic --backend docker --image sha256:<image-id> --arch arm64
```

Use `--arch amd64` for amd64 images.
An exact registry digest already provisioned in the backend is also accepted; mutable tags are not.
Success requires prompt settlement and disposable environment removal.
`--namespace` defaults to `omnigrex-runtime-state` for Docker and `omnigrex` for Microsandbox.
Set it to the actual installation runtime-state volume/native namespace if changed; exact namespaced cleanup cannot collect another installation's probe.

For Microsandbox, build the [native binary/image](development.md#build-the-images), provision the diagnostic image in a separate native cache, and run `pi-diagnostic --backend microsandbox --image REGISTRY/IMAGE@sha256:DIGEST --arch amd64`.
`--microsandbox-home` defaults to `/tmp/msb-pi`; executable/firmware options select the installed pinned pair.
The same non-root Linux/KVM requirements apply.

Configuration qualification uses the real authenticated gateway with disposable fixture authority and an in-memory backend:

```sh
mise exec -- go run ./cmd/omnigrex pi-diagnostic --backend docker --image sha256:<image-id> --arch arm64 --configuration
mise exec -- go run ./cmd/omnigrex pi-diagnostic --backend docker --image sha256:<image-id> --arch arm64 --configuration --reviewer
mise exec -- go run ./cmd/omnigrex pi-diagnostic --backend docker --image sha256:<image-id> --arch arm64 --sessions
```

Use the corresponding native backend arguments on Microsandbox.
Configuration and auth travel over process transport rather than environment metadata; only selected fixture provider auth enters private tmpfs.
`--sessions` verifies exact file-backed identity, fenced sequencing, and replay-before-human-prompt with temporary state and no durable Workflow records.
Shared gates additionally test continuation across recreated environments and independent retained-state attachments; PostgreSQL tests verify reservation/admission separately.
See [protocol evidence](../plans/2026-10-09-pi-protocol-adapter.md), [configuration evidence](../plans/2026-10-09-pi-configuration-and-mcp.md), and [Session evidence](../plans/2026-10-09-durable-pi-sessions.md).

Automated protocol gates build/transfer their fixtures without client host mounts:

```sh
mise run test-pi-integration
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-integration
```

## Live-provider smoke

`test-pi-live` submits a short real prompt per provider through production `pi-rpc/v2`, the golden image, owned runner, checked renderer, and selected backend.
It creates isolated storage and fixture authenticated MCP discovery and audits stopped Session archives for credentials.
It creates no Workflow or GitHub mutations.

Supply a private Pi-native literal API-key file containing `openai` and `opencode-go` entries using [secret handling](../operators/secrets.md).
The script streams it into disposable runner tmpfs rather than Docker arguments, environment metadata, layers, or persistent fixture storage.
Only the selected entry enters normal stdin bootstrap and `/run/pi-agent` tmpfs.

```sh
OMNIGREX_PI_LIVE_AUTH_FILE=/secure/path/pi-auth.json \
  OMNIGREX_PI_BACKEND=docker mise run test-pi-live
OMNIGREX_PI_LIVE_AUTH_FILE=/secure/path/pi-auth.json \
  OMNIGREX_PI_BACKEND=microsandbox OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> \
  mise run test-pi-live
```

Default models are `openai/o4-mini` and `opencode-go/muse-spark-1.3-contributor`, with checked `low` thinking.
Override IDs using `OMNIGREX_PI_OPENAI_MODEL` or `OMNIGREX_PI_OPENCODE_MODEL` within those providers.
Unavailable models or unsupported thinking fail without fallback.
Calls use real service and may incur charges; this is separate from deterministic CI.
Record actual image/backend/model and observed outcomes rather than relying on feasibility evidence.
OAuth/subscription snapshots are unsupported by this API-key gate.

### ChatGPT subscription qualification

Use a dedicated browser-authorized qualification registration rather than a copied production refresh token.
The gate builds isolated backend resources, starts the normal Omnigrex login command, prints a browser URL, and accepts the full loopback redirect through stdin when the browser is remote.
The registration lives only in the isolated controller-side volume and is logged out and remotely revoked on completion where possible.
No OpenAI API key is used or accepted as fallback.

```sh
OMNIGREX_PI_BACKEND=docker mise run test-pi-subscription-live
OMNIGREX_PI_BACKEND=microsandbox OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> \
  mise run test-pi-subscription-live
```

The default model is `openai/gpt-6.1-sol`; set `OMNIGREX_PI_OPENAI_MODEL` to an exact pinned model available to the account if necessary.
Each smoke requires real inference, an isolated MCP fixture-tool call, another model request carrying the tool result, and an audit of stopped Session archives for issued credentials.
It performs no real GitHub reads or mutations.
The native command needs the same Linux/amd64 KVM prerequisites as other native gates.
Record both actual backend/image/model results before claiming subscription release qualification.

Deterministic authentication-manager tests cover refresh serialization, idle renewal, persistence, interrupted renewal, account replacement/logout races, and independent recovery blocks.
The Pi failure-boundary fixture also runs the subscription request/tool-history, per-request token replacement, error-sanitization, and no-ambient-key-fallback tests on both backend suites.

## Complete Agent Turns and recovery

Use fresh isolated database/storage and actual registry-pinned production environments for release qualification.
Existing protected Participants keep original bindings.
See [image provisioning](../operators/installation.md#3-resolve-and-provision-images) and [Agent Profile configuration](../repository-owners/configure-agents.md).

```sh
mise run test-pi-workflow
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-workflow
mise run test-pi-recovery
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-recovery
```

The controlled-provider suites create independent PostgreSQL/network/volume resources and remove only their own resources.
They do not recreate the stable development stack or mutate external GitHub repositories.
Docker controller wrappers use the daemon socket, while Pi environments never receive it.
Native wrappers use KVM and transferred image archives without a host Docker socket.
Fixture images remain cached.
See [Agent Turn evidence](../plans/2026-10-09-complete-pi-agent-turns.md) and [recovery evidence](../plans/2026-10-10-pi-failure-recovery.md).

## Pi upgrade and state recovery qualification

Qualify source-to-candidate Session Continuation and History Replay on isolated copies before changing the configured image.
Retained Participants do not rebind.
Read [upgrade evidence](../plans/2026-10-10-pi-upgrades-and-state-recovery.md) and [backend-scoped qualification policy](../decisions/0017-bind-pi-qualification-to-backend-contracts.md).

```sh
mise run test-pi-upgrade
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-upgrade
```

Fixture gates use controlled provider/MCP servers, private volumes, and independently built source/candidate inventories.
Synthetic aliases are test evidence and cannot emit deployment artifacts.

For actual release qualification, provision both exact registry images and choose an absent private absolute output path:

```sh
OMNIGREX_PI_UPGRADE_SOURCE_IMAGE='REGISTRY/PI@sha256:SOURCE_DIGEST' \
OMNIGREX_PI_UPGRADE_TARGET_IMAGE='REGISTRY/PI@sha256:CANDIDATE_DIGEST' \
OMNIGREX_PI_UPGRADE_SOURCE_VERSION=1.0.3 \
OMNIGREX_PI_UPGRADE_TARGET_VERSION=1.0.4 \
OMNIGREX_PI_UPGRADE_SOURCE_CONTRACT_FILE='/private/absolute/source-runtime-contract.json' \
OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE='/private/absolute/pi-compatibility.json' \
mise run test-pi-upgrade
```

Use actual versions and the native task/KVM requirements for Microsandbox.
The candidate must satisfy the owned golden-resource contract; a changed Node/resource contract needs explicit implementation and qualification.
Supply the trusted canonical source contract whenever immutable fields differ from fixture defaults, including a historical 128-PID budget even when image/resources match.
It must bind the actual source image/platform and approved inventory.
Source, candidate, and restored-source launches validate their own immutable inventories before prompting.

Only the controlled provider fixture is injected into execution; results identify real source/candidate images.
The gate does not automatically pull them or use real credentials.
Publication requires a complete pass, refuses overwrite, validates staged JSON or checksummed Session archives, and atomically writes mode-0600 output.
Failed transfer/validation leaves no partial final artifact.
Use version-2 backend-scoped results in the [operator upgrade procedure](../operators/upgrades.md#runtime-profile-image-change).
Direction matters and does not establish rollback compatibility.

### Compatible-host restore drills

Production recovery requires coordinated [database/state/capture backup](../operators/backup.md) plus the native journal where applicable.
Restore the exact ledger, namespace, and source images before Participant archives and fence the original host.

For fixture-only two-host drills, set `OMNIGREX_PI_UPGRADE_EXPORT_ARCHIVE` to an absent private client-side path on the source gate.
Transfer exact fixture images using Docker image save/load.
On the compatible replacement, set `OMNIGREX_PI_UPGRADE_USE_CACHED_FIXTURES=1` and `OMNIGREX_PI_UPGRADE_RESTORE_ARCHIVE` to the archive and rerun the gate.
Platform changes, different Participants/contracts/images, namespace remapping, and cross-backend restore are rejected.
Fixture archives do not substitute for PostgreSQL and protected captures.

## Image-only deployment smoke

Run `mise run test-pi-deployment` or `mise run test-pi-microsandbox-deployment` on the daemon host.
They stage deployment files and fixture secrets in a separate no-clone directory with unique resources.
The suites test authenticated/deduplicated durable webhooks, invalid signatures, unsupported events, persistence, network isolation, liveness/readiness failures, doctor, and coordinated recovery including native journal when applicable.
Fixture App installation checks intentionally fail, and no real repository is mutated.
Native smoke additionally proves startup rejects missing KVM.
Synthetic archive markers prove ownership/round-trip, while Workflow/upgrade gates prove actual continuation/replay.

Set both `OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE` and `OMNIGREX_PI_DEPLOY_IMAGE` to actual registry digests to qualify published images without builds.
These variables affect qualification scripts only; normal deployment uses `OMNIGREX_ORCHESTRATOR_IMAGE` and `OMNIGREX_PI_IMAGE`.
CI requires Docker and usable Linux/amd64 KVM, then repeats smoke after publication against resolved digests.

# Operator Guide

This guide describes the supported first-iteration deployment and recovery procedures for Omnigrex.
Read the trust model before installing credentials or granting Docker access.

## Supported Deployment

The Pi deployment consists of one Docker Compose control-plane stack running the orchestrator and PostgreSQL, with disposable Pi Runtime Processes on the selected Docker or Microsandbox Execution Backend.
OpenCode remains a preparatory compatibility path until Step 17; the one-time transition uses the [clean-cutover procedure](#one-time-opencode-to-pi-cutover), never a conversation or populated-ledger migration.
Docker Engine API 1.45 or newer and Docker Compose v2 are required.
The operator must provide public DNS and HTTPS ingress for GitHub webhooks.
Production hosts deploy without cloning the repository by using published `linux/amd64` images from `ghcr.io/jozala/omnigrex/` and the [Pi deployment files](#pi-operational-deployment) for one qualified commit.
A source checkout is only needed for development and for qualifying Runtime Profile upgrades elsewhere.

Only trusted repositories are supported.
The Docker control plane has effective host-root authority through the Docker socket and can read all deployment secrets.
The Microsandbox control plane receives KVM access and does not mount the host Docker socket.
Pi Runtime Processes do not receive GitHub credentials or the host Docker socket, but they receive the selected provider's real API key and outbound network access.
A malicious repository, dependency, plugin, instruction file, or agent command can expose provider credentials.
Different Roles may select providers from the same deployment bundle; only the selected entry enters each Execution Environment.
Narrow provider privileges, configure spending limits, and rotate the complete bundle after any suspected repository compromise.

Omnigrex does not enforce required GitHub checks before `omnigrex:pr-ready`.
Use GitHub branch protection when checks must be mandatory.

### Backend selection and guarantees

Set `OMNIGREX_EXECUTION_BACKEND` through exactly one Pi backend overlay for the entire installation.
There is no silent fallback, mixed-backend routing, or automatic Docker-to-Microsandbox retained-state conversion.

| Guarantee or requirement | Docker | Microsandbox |
| --- | --- | --- |
| Host | Docker Engine on Linux or Docker Desktop on macOS | Linux/amd64 with usable KVM and the native control-plane image |
| Isolation claim | Process/container isolation for trusted repositories | Hardware-isolated microVM execution |
| Repository tools | Working-checkout mise installation inside execution | Same |
| Docker, Compose, Testcontainers inside an Agent Turn | Unsupported required capability | Qualified private guest daemon; no host socket |
| Whole-environment stop, restart discovery, exact-epoch cleanup | Required | Required, including nested workloads |
| Consistent captures, retained Session ownership, continuation/replay | Required | Required |
| Resource evidence | Container limits and Docker-reported exit/OOM evidence | Guest limits; unknown crash/writeback evidence stays unknown |
| Provider-key mediation | Not implemented | Not implemented; guest code can read supplied keys |

Both backends reject unsupported requested capabilities before repository execution.
Nested microVM execution is investigated, not a release gate.
Backend selection does not change Workflow, Role, MCP, or trusted publication policy.

## Microsandbox execution backend

The Linux/amd64 KVM implementation pins Microsandbox and Go SDK 0.7.6.
Its implementation and qualification boundaries are recorded in the [Step 08 evidence](./plans/2026-10-09-microsandbox-backend.md).
The [Pi operational deployment](#pi-operational-deployment) section describes the backend-selected image-only bundles and KVM CI gates introduced in Step 15.

Build the native orchestrator from a source checkout on a Linux/amd64 builder:

```sh
docker build --file Dockerfile.microsandbox --target runtime --tag omnigrex/orchestrator-microsandbox:dev .
```

This image includes the pinned native runtime, firmware, SDK shared library, and CGO-enabled orchestrator.
It also includes checksum-locked Git, its HTTPS helper, and the required Ubuntu dependency closure for trusted repository preparation.
The normal `Dockerfile` remains the static Docker backend build.
Selecting Microsandbox in a binary without its SDK support fails explicitly.

Select one backend with `OMNIGREX_EXECUTION_BACKEND=docker` or `microsandbox`.
Existing PostgreSQL backend ownership and namespace bindings are immutable; changing this setting does not migrate or reinterpret retained Participants.
Use a fresh installation or an explicitly designed disposal/migration procedure for a backend change.

Native execution requires the control-plane process to run as UID/GID 10001 with read/write KVM device access.
Grant its supplementary group the numeric ownership of `/dev/kvm`; do not expose the host Docker socket to the microVM.
Provide enough host capacity for all admitted VMs and their VMM overhead.
Docker/Compose/Testcontainers inside an Agent Turn use the guest's private daemon, whose socket does not grant host Docker authority.
Guest-root code can modify guest controls and access real provider credentials supplied to that environment.
Continue using the credential scope and rotation procedures in [Supported Deployment](#supported-deployment) and [Prerequisites](#prerequisites).

Native configuration includes:

| Variable | Meaning |
| --- | --- |
| `OMNIGREX_MICROSANDBOX_EXECUTABLE` | Installed pinned `msb` executable; image default `/usr/local/bin/msb` |
| `OMNIGREX_MICROSANDBOX_FIRMWARE` | Installed firmware library; image default `/usr/local/lib/libkrunfw.so.5.6.1` |
| `OMNIGREX_MICROSANDBOX_HOME` | Backend inventory, image cache, disposable VM resources, and private dispatch journal; default `/var/lib/omnigrex/msb` |
| `OMNIGREX_MICROSANDBOX_NAMESPACE` | Immutable installation inventory/state namespace; default `omnigrex` |
| `OMNIGREX_MICROSANDBOX_CPUS` | VM virtual CPUs, 1-64; default 2 |
| `OMNIGREX_MICROSANDBOX_ROOT_DISK_MIB` | Disposable root capacity, 1,024-1,048,576 MiB; default 20,480 |

Keep the native home path at most 40 bytes to preserve the pinned runtime's Unix-socket path budget.
Its directory must be private to the control plane and disjoint from workspace, state, and tool roots.
The existing logical workspace, retained-state, capture, and tool-root configuration remains applicable.
Use separately retained storage for Session data and captures, with matching directory ownership.
Do not bind the native home or entire storage roots into a guest.

Provision exact repository images into the selected native image cache before accepting work, using the installed `msb image pull` or image archive import commands.
Admission verifies actual manifest/config content and platform linkage; an imported digest-shaped alias is not sufficient proof of image identity.
Index-pinned images and SDK-reserialized registry manifests require bounded HTTPS access to their exact registry artifacts for verification.
Ordinary execution and golden-resource diagnostics use cached layered artifacts without pulling.
Repositories requesting nested containers require flat ext4 materialization; the SDK may fetch the same declared registry digest within the bounded preparation operation to create that flat artifact.
Keep those exact registry artifacts available as described in [Repository Environments](#repository-environments).
No repository image build or mutable-tag fallback occurs inside Omnigrex.

The native preflight checks the installed runtime version, local SDK selection, KVM access, selected images, isolated golden resources, and the real authenticated diagnostic MCP path.
Use the existing `doctor` command and check every reported prerequisite.
VM CPU/memory allocations and workload PID limits have guest scope; they are not host VMM-thread limits.
Transport loss, cancellation, an exit code of 137, and unknown guest exits do not establish OOM or successful cleanup.
After an unflushed VM crash, required terminal workspace evidence may remain unavailable, and settlement preserves that uncertainty.
An unresponsive guest that cannot complete writeback is still force-terminated by exact identity under a finite cleanup budget.
Such termination does not authorize discarding started execution data or claiming successful terminal capture.
The SDK's crashed state is terminal for writer exclusion and preparation-only cleanup, but it supplies no missing writeback or OOM evidence.

Apply the coordinated [backup](#backup) and [restore](#restore) procedures to PostgreSQL, retained Session data, and protected Workspace Captures.
Also preserve the native `omnigrex-operations` journal under its home directory, keeping the original host fenced off during compatible-host restore.
Exclude disposable VM root disks, memory snapshots, and guest tmpfs credentials from ordinary runtime-state archives.
The `state export` and `state restore` commands select the same backend and retain the existing offline writer/collection barrier.
Cross-backend archives are rejected.
Retained dispatch journals permit recovery cleanup, but a restarted control plane cannot adopt a prepared environment's prior ephemeral registration inputs.

Run the backend gate against an approved Linux Docker host with usable KVM:

```sh
export OMNIGREX_MICROSANDBOX_KVM_GID="$(stat -c '%g' /dev/kvm)"
mise run test-microsandbox-integration
```

For a remote host, set an existing authorized `DOCKER_HOST=ssh://USER@HOST` connection and obtain that host's KVM group.
The gate transfers fixture images over stdin and uses a disposable non-root wrapper rather than client-side host mounts.
It removes that wrapper and the VMs it owns, while fixture images remain in the daemon's image cache.

## Pi protocol diagnostic

Step 09 introduces a disposable Pi 1.0.4 protocol diagnostic while OpenCode remains the default runtime.
Its [implementation evidence](./plans/2026-10-09-pi-protocol-adapter.md) describes the supported boundary and subsequent integration steps.
The diagnostic accepts no real provider credentials and uses a controlled HTTP provider inside its disposable environment.
It has no GitHub or business MCP authority, does not create a Workflow, and stores its temporary Session in tmpfs.

Build the pinned diagnostic image and resolve its content-addressed local identity:

```sh
mise run pi-diagnostic-image
docker image inspect omnigrex/pi-diagnostic:step09 --format '{{.Id}} {{.Architecture}}'
```

Pass that exact image ID and architecture to the source-built command for Docker:

```sh
mise exec -- go run ./cmd/omnigrex pi-diagnostic --backend docker --image sha256:<image-id> --arch arm64
```

Use `--arch amd64` for Linux/amd64 images.
An exact registry digest is also accepted when that artifact is already available to the selected backend.
The command never selects a mutable image tag or submits work to a live model.
It reports success only after Pi admits and settles the controlled prompt and the disposable environment is stopped and removed.
`--namespace` identifies the owning installation, defaulting to `omnigrex-runtime-state` on Docker and `omnigrex` on Microsandbox.
Set it to the deployment's runtime-state volume name on Docker or configured Microsandbox namespace when using different installation names.
Startup inventory and exact-identity cleanup cannot collect another installation's namespaced protocol probe.

For Microsandbox, build the native binary using the [existing backend procedure](#microsandbox-execution-backend) and provision the exact diagnostic image in a separate native image cache.
Run that binary with `pi-diagnostic --backend microsandbox --image REGISTRY/IMAGE@sha256:DIGEST --arch amd64`.
The default private diagnostic cache is `/tmp/msb-pi`; override it with `--microsandbox-home` when required.
`--microsandbox-executable` and `--microsandbox-firmware` select the installed pinned runtime pair.
The same Linux/KVM and non-root identity requirements apply.

The automated backend gate builds and transfers its own fixture image archive without host mounts:

```sh
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-integration
```

Use the existing approved `DOCKER_HOST` connection for a remote Linux/KVM host.
The wrapper is disposable and receives KVM access without granting its guest access to the host Docker socket.
Fixture images remain cached on the build daemon.

Step 10 adds the owned runner's checked configuration and authenticated MCP qualification to the same two backend gates.
The [configuration evidence](./plans/2026-10-09-pi-configuration-and-mcp.md) describes the candidate Profile schema and instruction/resource contracts.
The configuration diagnostic uses the real gateway with disposable fixture authority and an in-memory backend; it has no GitHub credentials or external mutations.
Provider configuration and MCP authority enter over the process transport rather than container or VM environment metadata.
Only the selected provider entry is written to a private Pi-native `auth.json` file in disposable `/run/pi-agent` tmpfs.

Run the owned configuration diagnostic against the exact image identity obtained above:

```sh
mise exec -- go run ./cmd/omnigrex pi-diagnostic --backend docker --image sha256:<image-id> --arch arm64 --configuration
mise exec -- go run ./cmd/omnigrex pi-diagnostic --backend docker --image sha256:<image-id> --arch arm64 --configuration --reviewer
```

For Microsandbox, use the native binary and exact provisioned image with the previously described backend arguments, adding `--configuration` and optionally `--reviewer`.
These preparatory diagnostics accept only fixture credentials; production Pi credential configuration and cutover follow the later deployment steps.

Step 11 adds a disposable Session-coordination diagnostic with exact file-backed identity and History Replay:

```sh
mise exec -- go run ./cmd/omnigrex pi-diagnostic --backend docker --image sha256:<image-id> --arch arm64 --sessions
```

For Microsandbox, use the native binary and exact provisioned image with the previously described backend arguments, adding `--sessions`.
This option exercises preparation, fenced prompt sequencing, and replay-before-human-prompt with an in-memory diagnostic admission ledger and temporary `/state` tmpfs.
It does not attach protected Participant state or create durable Workflow records.
The shared automated backend gates additionally qualify actual continuation across removed and recreated Execution Environments with independent retained-state attachments.
PostgreSQL integration tests verify durable identity reservation and admission fencing separately.
See the [Step 11 evidence](./plans/2026-10-09-durable-pi-sessions.md) for the precise delivery boundary and acceptance coverage.

## Candidate Pi Agent Turn qualification

Step 12 exposes explicit `pi-rpc/v1` Agent Turns for qualification before the clean cutover.
The [implementation evidence](./plans/2026-10-09-complete-pi-agent-turns.md) describes the precise delivery boundary.
Use a disposable deployment and fresh database/storage for this qualification; existing retained Participants keep their original runtime/image bindings.

Build the production candidate environment:

```sh
mise run pi-image
```

Build/publish repository environments outside Omnigrex and configure their actual registry digest using the existing [image and deployment procedures](#deploy).
Set both `OMNIGREX_PI_RPC_V1_IMAGE=REGISTRY/IMAGE@sha256:DIGEST` and `OMNIGREX_PI_RPC_V1_PLATFORM=linux/amd64` or `linux/arm64` in the disposable deployment configuration.
Both settings are optional together and required together; mutable tags, incomplete bindings, and unsupported platforms are rejected.
Explicit repository Profiles use `runtime: pi-rpc/v1`, an exact `provider/model`, optional supported thinking, and Pi permissions described in the [configuration evidence](./plans/2026-10-09-pi-configuration-and-mcp.md#candidate-agent-profile-schema).
The candidate credential bundle uses Pi-native literal API-key entries, with only the selected provider written into disposable runtime storage.
Follow the existing secret placement and recovery procedures in this guide; keep provider keys and deployment metadata out of source control and retained-state backups.
Backend selection uses the existing `OMNIGREX_EXECUTION_BACKEND` setting and the applicable backend prerequisites.
Step 15 adds Pi-selected operational diagnostics and release publication; the operator-controlled cutover remains a later milestone.

Run the controlled complete Workflow gates without deployment secrets or external GitHub mutations:

```sh
mise run test-pi-workflow
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-workflow
```

Use the approved `DOCKER_HOST` connection for a remote Linux/KVM host.
Each gate creates independent temporary PostgreSQL, network, and volume resources and removes only its own resources afterward.
These gates do not recreate the stable development Compose stack.
The Docker gate's outer control plane accesses the daemon socket to manage scoped environments; the Pi environment never receives it.
The Microsandbox wrapper receives KVM access and imports the fixture image without a host Docker socket.
Fixture images remain cached on the selected build daemon.

Step 13 runs the shared failure-recovery matrix against the same isolated qualification topology:

```sh
mise run test-pi-recovery
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-recovery
```

Use the same approved Linux/KVM `DOCKER_HOST` connection for the native gate.
The [recovery evidence](./plans/2026-10-10-pi-failure-recovery.md) records the fault scenarios, conservative uncertainty handling, and dependency enforcement.

## Pi operational deployment

The image-only Pi bundles support a fresh installation or isolated qualification deployment.
Read the [Step 15 evidence](./plans/2026-10-11-pi-operational-integration.md) for verification and release status.
Do not apply these bundles to populated OpenCode state; follow the [one-time cutover](#one-time-opencode-to-pi-cutover).
The existing OpenCode bundle remains available through the preparatory period.

The release workflow publishes `linux/amd64` Pi 1.0.4 / Node 24.14.1 golden environments at `ghcr.io/jozala/omnigrex/pi:sha-<commit>` and the SDK-enabled control plane at `ghcr.io/jozala/omnigrex/orchestrator-microsandbox:sha-<commit>`.
Docker uses the existing `ghcr.io/jozala/omnigrex/orchestrator:sha-<commit>` image.
Publication depends on both the Docker gate and the required Linux/amd64 KVM gate, and the workflow summary records every resolved registry digest.
No Pull Request run publishes images.
A package administrator must make new packages public and confirm unauthenticated pulls before advertising no-login deployment support.

Download the following files from one successfully published commit using the [commit-pinned distribution procedure](#published-images-and-deployment-files):

- `deploy/compose.pi.yaml`, the common PostgreSQL, Pi image validator, and orchestrator services.
- Exactly one of `deploy/compose.pi-docker.yaml` and `deploy/compose.pi-microsandbox.yaml`.
- `deploy/.env.pi.example`, copied to the private deployment `.env`.

For example, stage one Docker deployment in an empty directory:

```sh
COMMIT=<full-successfully-published-commit-SHA>
mkdir omnigrex-pi-deploy
cd omnigrex-pi-deploy
curl -fsSLO "https://raw.githubusercontent.com/jozala/omnigrex/${COMMIT}/deploy/compose.pi.yaml"
curl -fsSLO "https://raw.githubusercontent.com/jozala/omnigrex/${COMMIT}/deploy/compose.pi-docker.yaml"
curl -fsSLO "https://raw.githubusercontent.com/jozala/omnigrex/${COMMIT}/deploy/.env.pi.example"
cp .env.pi.example .env
chmod 0600 .env
install -m 0700 -d secrets
export COMPOSE_FILE=compose.pi.yaml:compose.pi-docker.yaml
```

For Microsandbox download `compose.pi-microsandbox.yaml` instead and select `COMPOSE_FILE=compose.pi.yaml:compose.pi-microsandbox.yaml`.
Keep that exact `COMPOSE_FILE` exported in every deployment, diagnostic, backup, restore, and rotation shell; add any instruction or storage override to the same file set.
The ordinary `docker compose` commands later in this guide assume this selection.
Leaving it unset can select the old `compose.yaml` or omit the backend overlay.
The default bundle volume and network names are fixed, so a different project name alone does not isolate a second installation.
Rehearsals must override every storage/network name and matching `OMNIGREX_*_VOLUME`, agent-network, and native namespace setting as the [isolated gate](#isolated-cutover-rehearsal) does.

There are no build contexts or source-checkout dependencies in these files.
Resolve the matching published tags to registry digests as described in [Published Images And Deployment Files](#published-images-and-deployment-files), set `OMNIGREX_ORCHESTRATOR_IMAGE` to the selected backend's control-plane digest, and set `OMNIGREX_PI_RPC_V1_IMAGE` and `OMNIGREX_PI_RPC_V1_PLATFORM=linux/amd64`.
Keep all exact retained Participant images available according to [Repository Environments](#repository-environments).

Use the existing [GitHub Apps](#github-apps), [secret-file placement](#secrets), and [HTTPS ingress](#supported-deployment) procedures.
For Pi, the provider file must use literal Pi-native API keys, for example `{"openai":{"type":"api_key","key":"REPLACE_WITH_SECRET"}}`, rather than OpenCode's `type: api` shape.
Only API keys are supported; executable key references, environment references, OAuth entries, and missing selected providers fail preflight.
Pi Profiles use `runtime: pi-rpc/v1`, `provider/model`, optional thinking, and the permissions described in the [candidate Profile schema](./plans/2026-10-09-pi-configuration-and-mcp.md#candidate-agent-profile-schema).
Set `OMNIGREX_PI_STARTUP_TIMEOUT` in the selected Compose deployment's `.env` to budget repository bootstrap, initial Pi configuration delivery, and initial RPC readiness (default `10m`).
The adapter starts this single deadline when it attaches to the Runtime Process; a shorter Agent Turn or diagnostic deadline still wins.
After the first validated `get_state` response, each subsequent configuration RPC uses its ordinary 30-second deadline while the remaining startup deadline bounds preparation overall.
The Agent Turn's overall timeout (default `2h`) begins before Runtime Process launch, includes startup and prompt execution, and leaves stop/capture cleanup under its separate budget; increasing it alone does not extend the startup allowance.
The INFO-level `Pi startup readiness started` and `Pi startup readiness completed` observations include Agent Turn and Session identifiers, budget, duration, and a safe outcome after both RPC and durable Session preparation; they do not diagnose a specific tool download or establish that an uncertain execution stopped.
Doctor uses a disposable environment and its own shorter outer diagnostic budget, so passing doctor does not prove that working-revision repository tools will install during a real Agent Turn.

### Docker bundle

Set the Docker socket and secret-file groups using [Secret File Group](#secret-file-group), then start with:

```sh
docker compose up -d --no-build --wait
docker compose exec orchestrator /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

The bundle requires Docker Engine API 1.45 or newer and no KVM device or native SDK libraries.
Docker-backed development on macOS remains supported with locally built arm64 images and `OMNIGREX_PI_RPC_V1_PLATFORM=linux/arm64`; the release images are initially amd64.
The host Docker socket is exposed only to the control plane, never to Pi Execution Environments.

### Microsandbox bundle

Use a Linux/amd64 host with usable `/dev/kvm` and the [Microsandbox resource and trust constraints](#microsandbox-execution-backend).
Set `OMNIGREX_MICROSANDBOX_KVM_GID` to the numeric ownership of that device and use a different dedicated secret-file group.
The control-plane memory limit must cover admitted VM memory plus VMM overhead, PostgreSQL, and preparation costs; the example uses 6 GiB for two 1 GiB Agent Turns.
The bundle grants KVM access without mounting the host Docker socket, enabling privileged mode, or granting extra Linux capabilities.

Provision the exact image into the native cache before starting the orchestrator:

```sh
docker compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator image pull 'REGISTRY/PI@sha256:DIGEST'
docker compose up -d --no-build --wait
docker compose exec orchestrator /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

Use the same exact declared digest, not a replacement tag or an imported digest-shaped alias.
The protected native-home volume contains cache artifacts and the dispatch journal; disk-backed `TMPDIR` under that home supports image import and materialization without overflowing control-plane tmpfs.
Preserve only the protected journal from this volume in the coordinated recovery set, following [Microsandbox backup requirements](#microsandbox-execution-backend), while retaining Session data and Workspace Captures through the existing backup procedure.
The native backend fails startup without usable KVM or exact image/resource compatibility before serving webhooks or admitting Agent Turns.
Requested unsupported execution capabilities fail explicitly without another-backend fallback.

### Diagnostics and backend gates

Startup verifies every configured Runtime Profile's golden resources before starting Workflow workers.
Readiness continuously checks the selected backend, PostgreSQL, configured and protected exact images, retained-state availability, and pending compatibility evidence.
Docker writable-path probes use the selected immutable Runtime Profile rather than OpenCode paths.
The healthcheck HTTP client uses the same `OMNIGREX_READINESS_TIMEOUT` as the readiness handler, plus a five-second response allowance, instead of a fixed twenty-second timeout.
The Pi bundle defaults to a 60-second readiness budget, a 65-second client deadline, and a 70-second Compose healthcheck deadline.
When changing the readiness budget, also set `OMNIGREX_HEALTHCHECK_TIMEOUT` at least ten seconds longer so Compose can receive the bounded client's result before terminating the probe.
The legacy Docker bundles retain the corresponding 15/20/25-second defaults and support the same outer-timeout override.

Doctor resolves the same configured registry as startup, checks repository environments for each Role's selected runtime, validates Pi credentials and effective instructions/permissions, and verifies selected Pi models and thinking values without fallback.
Its `pi-runtime` check starts the production owned runner with a random exact Session identity in temporary `/state`, fixture-only provider auth, and isolated authenticated diagnostic MCP authority.
It prepares but submits no model prompt, does not invoke business capabilities, and removes the disposable environment before reporting success.
Diagnostic Session files and authority never attach to retained Participant state.
For mixed preparatory runtime configuration, doctor checks every configured runtime and reports `agent-runtime`; an OpenCode-only deployment retains `acp-runtime`.

The complete source-checkout backend gates are:

```sh
mise run test-docker-integration
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-microsandbox
```

The component conformance, Pi protocol, Workflow, recovery, and upgrade gates remain separately runnable as documented above, and can use an approved remote `DOCKER_HOST`.
The image-only smoke entry points, `test-pi-deployment` and `test-pi-microsandbox-deployment`, must run on the daemon host because Compose secrets are host-side files.
They stage only deployment files and disposable fixture secrets into a separate no-clone directory, use unique resource names, verify liveness/readiness and operational doctor checks, and remove only their own resources.
External GitHub installation checks intentionally fail for fixture App identities; no live repository is mutated.
The Microsandbox smoke additionally proves that removing KVM access rejects normal startup.
Set both `OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE` and `OMNIGREX_PI_DEPLOY_IMAGE` to actual registry digests to run the same smoke against published images without local builds.
CI runs the shared suites on Docker and on an ephemeral `ubuntu-24.04` KVM runner, fails if the device is absent or unusable, and repeats image-only smoke against the published digests after publication.

## One-time OpenCode-to-Pi cutover

This is one explicit reset of OpenCode operational state and conversations, followed by a fresh Pi installation.
It implements [decision 0006](./decisions/0006-use-pi-through-an-agent-specific-adapter.md), not an in-place runtime upgrade.
Step 17 owns the Pi-only release and operator-controlled production cutover; Step 16 rehearses the procedure using isolated resources.

### Preparation and shutdown

1. Select a release commit with successful Docker and KVM qualification, published exact Pi/control-plane images, public package pulls, and both exact-digest deployment smokes.
   Save the previous application revision, Compose files, `.env`, operator instructions, Profile revisions, and all exact protected image references in the private recovery metadata.
2. Rehearse on a separate host or with fully overridden installation resources using [the isolated gate](#isolated-cutover-rehearsal).
   Choose the Pi backend and provision its exact images and host prerequisites before touching the old installation.
3. Stop webhook ingress and pause human triggers for the maintenance window.
   Let admitted GitHub mutations settle; unresolved publication, stop, or corroboration evidence is a blocker to reset.
4. Using the old Compose file set, complete the [coordinated backup](#backup) and verify its checksums and archive inventory.
   Preserve old secrets, including the old database password and provider bundle, in encrypted storage separately from operational exports.
   Keep the old deployment stopped and verify no managed execution remains.
5. Record active Issues, Pull Requests, published heads, and uncertain external operations for human disposition.
   GitHub content and labels survive the reset, while Workflow history, Participants, Sessions, Attempts, budgets, idempotency records, pending jobs, and internal handoffs do not.
   Do not interpret old state labels or a pending redelivery as evidence of a Pi Workflow.

### Explicit disposal and fresh deployment

After the stopped-state backup passes, inspect the old selected Compose configuration and confirm the exact installation resource names.
For the standard old bundle the explicit disposal command is:

```sh
# Run only in the old deployment directory with its old Compose file set.
docker compose config --format json | jq -r '.volumes[] | .name'
docker compose down --volumes --remove-orphans --timeout 60
docker volume inspect omnigrex-postgres omnigrex-runtime-state omnigrex-workspaces omnigrex-mise
```

The final inspection must report that every old volume is absent; an existing volume or inspection transport failure is not successful disposal.
Use the recorded names for customized installations and inspect any external volumes separately because Compose does not remove external volumes.
Never apply this disposal command during an ordinary upgrade, backup, or restore.
Do not restore the OpenCode dump, Session data, captures, compatibility artifact, or native metadata into the fresh Pi installation.

Prepare the target deployment directory using [Pi operational deployment](#pi-operational-deployment), export its exact Compose file set, and replace the provider secret with the Pi-native API-key bundle.
Use fresh PostgreSQL, runtime-state, workspace, and tool volumes; for Microsandbox also use a fresh native home/journal and namespace.
Reusing stable default volume names is allowed only after confirmed disposal.
Changing only a project name or backend setting is insufficient.

Update all managed repositories' default-branch Profiles to `runtime: pi-rpc/v1`, translate local permissions, remove `variant` and `steps`, and select actual supported `provider/model` and optional thinking values.
Publish required checkout-local skills alongside those Profiles and ensure any intended Change Proposal checkout contains them.
Replace OpenCode-derived repository environment images with externally built and qualified Pi-derived images, then provision their exact digests.
Keep old Profile revisions privately recorded if a pre-Pi rollback may be required.

Start the selected bundle with `docker compose up --no-build --wait`, verify `/healthz` and `/readyz`, and require every repository `doctor` check to pass.
Restore ingress and verify a real authenticated Developer App webhook delivery receives HTTP 202 before accepting new triggers.
Handle old delivery retries and labels during the maintenance window so they cannot accidentally start fresh work.
For a retained old Issue or Pull Request, choose a deliberate human disposition; the new ledger cannot automatically adopt its old Participant, branch, review, or idempotency ownership.
Start a small fresh controlled Work Item and confirm Developer publication, Reviewer approval, and human review readiness.
Take the first coordinated Pi backup once that Work Item is quiescent.

### Rollback limits

Before any fresh Pi work or external mutations, rollback can restore the entire stopped OpenCode recovery set, previous application/configuration, credentials, exact images, and matching repository Profile/environment revisions.
Fence and dispose the failed fresh installation first so only one installation owns webhook and prompt admission.
Restoring an old database beneath a new application or keeping Pi state with an OpenCode ledger is unsupported.

After Pi begins fresh work, an OpenCode restore loses that operational history and cannot undo branches, Pull Requests, comments, or reviews already published to GitHub.
There is no Pi-to-OpenCode conversation conversion, ledger merge, or automatic reconciliation of two independently reset installations.
Prefer forward repair of the Pi deployment or a coordinated Pi restore; a deliberate return to OpenCode requires stopping Pi, preserving its recovery set, and human reconciliation of external effects before restoring the old set.
Later Pi image qualification is directional, backend-scoped, and performed on copies; it does not establish reverse compatibility or authorize retained-image rebinding.
After forward-only schema migrations, application rollback requires the matching pre-upgrade recovery set rather than an in-place database downgrade.

### Isolated cutover rehearsal

Run on the Docker daemon's host in a source checkout, using only disposable fixture credentials:

```sh
mise run test-pi-cutover
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-cutover
```

The equivalent host command without a mise installation is `OMNIGREX_PI_CUTOVER_REHEARSAL=1 OMNIGREX_PI_BACKEND=docker sh scripts/test-pi-deployment.sh`, or `OMNIGREX_PI_BACKEND=microsandbox` with its KVM group supplied.
A remote `DOCKER_HOST` alone is insufficient because Compose secrets are host-side files; stage the source on the daemon host and run there.
The gate isolates ambient configuration, creates unique networks/volumes/namespaces, starts the actual OpenCode stack, seeds synthetic disposal markers, validates a pre-reset dump/archive, removes the old resources, and starts the fresh Pi bundle.
It verifies readiness and independent doctor/MCP checks without paid model calls or real GitHub mutation authority.
It then checks coordinated Pi dump/state/capture recovery, numeric ownership, native-journal inclusion, and readiness after recreated containers.
Failure to back up or remove legacy storage stops the cutover before Pi startup.
All fixture resources and temporary secrets/backups are removed afterward; source images remain cached.

For exact published-image rehearsal, set both `OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE` and `OMNIGREX_PI_DEPLOY_IMAGE` to target registry digests, and both `OMNIGREX_PI_CUTOVER_SOURCE_ORCHESTRATOR_IMAGE` and `OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE` to the old release's registry digests.
The Microsandbox target uses its native control plane, while the old OpenCode source uses its Docker control plane.
Without those pairs the gate builds source fixtures, which do not establish publication or no-login release evidence.

The archive markers in this procedure test operational round-trip and disposal mechanics, not Pi conversation correctness.
Also run `test-pi-workflow` with `OMNIGREX_PI_WORKFLOW_TEST_PATTERN='^(TestPiControlledWorkflow|TestPiProtectedStateRecovery)$'` and `test-pi-upgrade`, or their Microsandbox equivalents, to qualify actual fenced Pi Sessions, protected ledger/state ownership, fresh-environment continuation, and replay without model activity.
External GitHub fixture failures are expected in the rehearsal; production requires all doctor checks, real webhook delivery, and the controlled Work Item above.
See [Step 16 evidence and release boundary](./plans/2026-10-11-pi-operator-documentation-and-cutover.md) for observed results and the remaining release gate.

## Pi upgrade and state recovery qualification

Step 14 adds exact Pi-to-Pi qualification before the clean cutover.
Read the [implementation evidence](./plans/2026-10-10-pi-upgrades-and-state-recovery.md) and [qualification decision](./decisions/0017-bind-pi-qualification-to-backend-contracts.md).
Retained Participants continue using their original image and immutable contract; qualification tests isolated Session copies.

Run the reproducible Pi 1.0.3-to-1.0.4 fixture gates:

```sh
mise run test-pi-upgrade
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-upgrade
```

Use the approved `DOCKER_HOST` connection for Linux/KVM qualification.
The gates use fixture credentials and controlled model/MCP servers, create private storage volumes, and collect their own disposable environments.
Their synthetic image aliases are executable test evidence and cannot emit deployment artifacts.

For release qualification, provision both actual exact registry images on the qualification Docker host and select an absent absolute output path:

```sh
OMNIGREX_PI_UPGRADE_SOURCE_IMAGE='REGISTRY/PI@sha256:SOURCE_DIGEST' \
OMNIGREX_PI_UPGRADE_TARGET_IMAGE='REGISTRY/PI@sha256:CANDIDATE_DIGEST' \
OMNIGREX_PI_UPGRADE_SOURCE_VERSION=1.0.3 \
OMNIGREX_PI_UPGRADE_TARGET_VERSION=1.0.4 \
OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE='/private/absolute/pi-compatibility.json' \
mise run test-pi-upgrade
```

Use the actual package versions and select `test-pi-microsandbox-upgrade` with its KVM prerequisites for native qualification.
The candidate must satisfy the deployment's owned Pi v1 resource contract; a different resource/Node contract needs an explicit implementation and qualification change.
For a historical source whose bundle differs from the included Pi 1.0.3 fixture, set `OMNIGREX_PI_UPGRADE_SOURCE_CONTRACT_FILE` to its trusted canonical Runtime Profile contract from the source release or recovery set.
That definition must bind the supplied source image/platform and include its approved golden-resource inventory.
Source, candidate, and restored-source launches undergo production resource validation against their respective immutable inventories before prompting.
The test injects only its controlled-provider fixture over the execution request, so the artifact identifies the actual source and candidate images rather than fixture-overlay images.
It does not pull images automatically or use real provider credentials.
Artifact publication requires a successful complete test and refuses to overwrite an existing file.
The source-checkout gate checks Docker copy completion, validates the staged compatibility JSON or checksummed Session archive, and publishes a mode-0600 file atomically on the destination filesystem.
Failed transfers or validation do not leave a partial final output path.

Transfer the private artifact and configure `OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE` through the existing [upgrade procedure](#runtime-profile-image-change), together with the matching candidate Pi digest/platform.
Startup and doctor validate every result against a configured exact Runtime Profile and the selected backend family/contract version before accepting it.
Keep the full canonical Runtime Profile definitions in the coordinated PostgreSQL recovery set; they preserve historical Node, mise, and owned-file checks across deployment upgrades.
Version-2 Pi results and version-1 OpenCode results use separate immutable ledger tables during preparation.
Qualification direction is significant and does not establish rollback compatibility.
Fresh Participants have independent Session state and retain the image-selection policy described in [Repository Environments](#repository-environments).

### Compatible-host Pi restore drills

Use the coordinated [backup](#backup) and [restore](#restore) procedures for a deployment recovery set.
The [supplemental Session export](#supplemental-session-exports) procedure also supports Pi's `pi-session/v3` state, through the selected backend and its offline writer barrier.
Restore the matching ledger, installation namespace, and exact source images on the replacement host before restoring protected Participant archives.
Keep the original host fenced off and preserve Microsandbox's protected journal as described in [Microsandbox execution backend](#microsandbox-execution-backend).

For a fixture-only two-host drill, set `OMNIGREX_PI_UPGRADE_EXPORT_ARCHIVE` to an absent private client-side file while running an upgrade gate on the source host.
Transfer the exact fixture images with Docker image save/load to the compatible replacement host.
Run the same gate there with `OMNIGREX_PI_UPGRADE_USE_CACHED_FIXTURES=1` and `OMNIGREX_PI_UPGRADE_RESTORE_ARCHIVE` pointing to the exported archive.
The replacement must support the archive's guest platform and the same backend contract; a platform change, foreign Participant, changed source image/contract, namespace remapping, and cross-backend restore are rejected.
Fixture export/import does not substitute for the production PostgreSQL and protected Workspace Capture recovery set.

## Tracing

The orchestrator exports OpenTelemetry traces over OTLP/HTTP with protobuf encoding when `OTEL_EXPORTER_OTLP_ENDPOINT` or `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is configured.
Tracing is inactive when neither endpoint is set, or when `OTEL_SDK_DISABLED=true`.
Both Compose bundles pass the tracing settings from the deployment `.env` to the orchestrator.

Configure an OTLP-compatible collector's HTTP endpoint in your private deployment `.env`:

```dotenv
OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.example:4318
OTEL_SERVICE_NAME=omnigrex
```

If the collector requires authentication, set `OTEL_EXPORTER_OTLP_HEADERS` using OpenTelemetry's URL-encoded `key=value` format.
Keep authentication headers in private deployment configuration alongside the other secrets described in [Prerequisites](#prerequisites).
Recreate the orchestrator container after changing tracing environment settings.
The generic endpoint automatically gets `/v1/traces` appended; `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, if set, is a complete trace URL and takes precedence.
`OTEL_EXPORTER_OTLP_TRACES_HEADERS` can override the generic headers for traces.
The exporter always uses OTLP/HTTP with protobuf encoding, so use the collector's HTTP endpoint rather than its gRPC endpoint.
`OTEL_EXPORTER_OTLP_PROTOCOL` is not needed and does not select a different exporter in this implementation.
For a collector running on the Docker host, use an address reachable from the orchestrator container; `127.0.0.1` inside the container refers to the container itself.
Docker Desktop provides `host.docker.internal`; on Linux Docker Engine, that name requires a host-gateway mapping in a Compose override, or use a reachable host IP address.
The collector must listen on an interface reachable from the container and have a trace receiver connected to its export pipeline.

Traces identify the service as `omnigrex` by default and include its build version.
Use `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES` to override the service name or attach environment attributes.
The default sampler is `parentbased_always_on`, which samples new root traces and respects upstream sampling decisions.
Set `OTEL_TRACES_SAMPLER=always_on` to sample every trace, or use `parentbased_traceidratio` with `OTEL_TRACES_SAMPLER_ARG=0.1` to sample 10% of root traces.

Instrumentation covers incoming webhook and MCP HTTP requests and outbound GitHub API requests, with W3C trace-context and baggage propagation.
Health and readiness requests are excluded from incoming tracing.
Docker clients for readiness probes and exact Runtime Profile image availability checks also suppress tracing, including probe cleanup.
Operational Docker clients retain tracing for Runtime Process operations.
Request bodies, tool arguments, and agent prompts are not added as trace attributes.
Claimed preparation, execution, webhook processing, mutation recovery, runtime cleanup, and outcome reconciliation create fixed-name operation spans.
Runtime launch and outcome reconciliation normally nest under execution, and GitHub/Docker client spans inherit the calling operation's context.
Deferred terminal corroboration creates a separate reconciliation attempt, including credentials and durable acknowledgement.
MCP tool operations cover admitted work through finalization, even after HTTP disconnection; a tool error can be recorded despite HTTP 200.
Expected blocked outcomes are distinct from infrastructure failures.
Asynchronous Workflow workers start independent traces; trace context is not persisted across durable queues or propagated across the Runtime Process MCP boundary.
Search structured `workflow_id`, `agent_participant_id`, `agent_session_id`, and `agent_turn_id` fields to connect independent traces.
JSON stdout records emitted with a valid operation context include `trace_id` and `span_id`, including unsampled contexts; a trace link may have no stored trace when sampling or delivery omitted it.
Collection should preserve JSON fields and keep domain and trace identifiers out of log stream labels.
There is no direct OpenTelemetry log export.
Long execution spans export on completion; operation-start logs and shorter child spans provide earlier visibility.
Published orchestrator builds identify their commit SHA, while unversioned local builds retain `dev`.
Completed spans are batched and flushed after services stop, within `OMNIGREX_SHUTDOWN_TIMEOUT`.
Compose allows one minute before forcibly stopping the orchestrator, configurable with `OMNIGREX_STOP_GRACE_PERIOD`.
Keep that grace period longer than service draining and Agent Turn cleanup plus a separate `OMNIGREX_SHUTDOWN_TIMEOUT` budget for exporting the final traces, increasing it when those timeouts are increased.
Exporter delivery failures are reported by the OpenTelemetry SDK without stopping Workflow processing.
Application metrics are separately opt-in as described below; direct log export is not enabled.

## Application Metrics

The orchestrator exports the bounded application instrument set over OTLP/HTTP with protobuf encoding.
There is no public metrics endpoint.
Both Compose bundles accept the same metrics settings, and both environment examples document them.

| Configuration | Traces | Application metrics |
| --- | --- | --- |
| No endpoints | Disabled | Disabled |
| Generic `OTEL_EXPORTER_OTLP_ENDPOINT` only | Enabled | Disabled |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` only | Enabled | Disabled |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` only | Disabled | Enabled |
| Generic endpoint with `OTEL_METRICS_EXPORTER=otlp` | Enabled | Enabled |
| Trace and metrics signal-specific endpoints | Enabled | Enabled |
| `OTEL_METRICS_EXPORTER=none` | Existing endpoint-based behavior | Disabled |
| `OTEL_SDK_DISABLED=true` | Disabled | Disabled |

`OTEL_METRICS_EXPORTER=otlp` requires a generic or metrics-specific endpoint; unsupported exporter selections fail startup.
The generic endpoint appends `/v1/metrics`; the metrics-specific endpoint is a complete URL and takes precedence.
`OTEL_EXPORTER_OTLP_METRICS_HEADERS` overrides generic OTLP headers for metrics, using the same URL-encoded format and private configuration rules as tracing.
Use HTTP(S) endpoints and header authentication, not credentials embedded in URLs.
The default `OTEL_METRIC_EXPORT_INTERVAL` is `60000` milliseconds; `OTEL_METRIC_EXPORT_TIMEOUT` defaults to `30000` milliseconds and `OTEL_EXPORTER_OTLP_METRICS_TIMEOUT` to `10000` milliseconds in Compose.
Configured timing values must be positive integers in milliseconds.
A longer interval reduces samples, not the number of distinct series.

Traces and metrics share service/build/resource identity.
SDK views exclude unplanned library metrics and drop non-allowlisted application dimensions.
Durable gauges use one read-only observation with a five-second deadline, including connection acquisition, and a four-second server statement timeout.
An observation failure omits all durable gauges for that collection and increments `omnigrex.observation.failures`; it never substitutes zeros or cached state.
Traces and metrics shut down concurrently under a fresh bounded shutdown context after services stop; the observation registration and database remain open until that finishes.
Delivery failures are reported using safe categories without stopping Workflow processing.

See the [application observability contract and deployment handoff](./observability/application.md) for instrument names, queue eligibility, series budget, dashboard/alert requirements, and outstanding deployment verification.

## Prerequisites

Install the following software on the deployment host:

- Docker Engine with API 1.45 or newer.
- Docker Compose v2.
- `curl` for downloading the pinned deployment files.
- `openssl` for secret generation, `jq` for selected resource inspection, `tar` for archives, and `shasum` for recovery-set checksums.
- A reverse proxy or tunnel that terminates publicly trusted HTTPS.

Git and `mise` are not required on a no-clone deployment host.
Use a separate source checkout with Git and `mise` only when qualifying a Runtime Profile upgrade.

Repository tools are installed inside the Agent Turn's execution environment from its working checkout, including compiling Go tools declared in `mise.toml`.
Allocate enough Docker host memory for the orchestrator, PostgreSQL, and the configured Agent Turn limits, including tool compilation.
Installation uses Participant-local disk-backed mise scratch, which is removed before the agent protocol starts.
The orchestrator does not contain mise or execute repository installation scripts.

Create the local configuration in the deployment directory:

```sh
install -m 0700 -d secrets
```

Do not commit `.env`, `secrets/`, provider credentials, private keys, compatibility artifacts containing deployment metadata, or backups.
See [Published Images And Deployment Files](#published-images-and-deployment-files) and [Deploy](#deploy) for how the deployment directory is created without cloning the repository.

## Published Images And Deployment Files

Every push to `main` that passes the Docker and Linux/KVM jobs publishes four `linux/amd64` images for discovery:

- `ghcr.io/jozala/omnigrex/orchestrator:sha-<full-commit-SHA>` from `Dockerfile`.
- `ghcr.io/jozala/omnigrex/opencode:sha-<full-commit-SHA>` from `agent/opencode/Dockerfile`.
- `ghcr.io/jozala/omnigrex/pi:sha-<full-commit-SHA>` from the golden target of `agent/pi/Dockerfile`.
- `ghcr.io/jozala/omnigrex/orchestrator-microsandbox:sha-<full-commit-SHA>` from the runtime target of `Dockerfile.microsandbox`.

No `latest` tag is published, and Pull Request runs publish no images.
After the first publication, a repository/package administrator must make all GHCR packages public and confirm that a host can pull them without `docker login`.
Do not add registry credentials to the deployment bundle.

Select a commit whose `main` CI run successfully published the matching images and passed both post-publication smoke jobs.
Open that run and read its `Published Omnigrex images` workflow summary, which records the commit SHA, `sha-<commit>` tags, and resolved `sha256` digests.

The Pi download and Compose selection procedure is in [Pi operational deployment](#pi-operational-deployment).
For the preparatory legacy OpenCode stack only, download these files into one empty host directory:

```sh
COMMIT=<full-commit-SHA>
mkdir -p omnigrex-deploy && cd omnigrex-deploy
curl -fsSLO "https://raw.githubusercontent.com/jozala/omnigrex/${COMMIT}/deploy/compose.yaml"
curl -fsSLO "https://raw.githubusercontent.com/jozala/omnigrex/${COMMIT}/deploy/.env.example"
cp .env.example .env
install -m 0700 -d secrets
```

There is no GitHub Release for every merge; the commit-pinned raw URLs are the distribution mechanism.

For Pi, pull the matching selected control-plane and golden-image tags and resolve their digests:

```sh
CONTROL_PLANE=orchestrator # use orchestrator-microsandbox for the native backend
docker pull --platform=linux/amd64 "ghcr.io/jozala/omnigrex/${CONTROL_PLANE}:sha-${COMMIT}"
docker pull --platform=linux/amd64 "ghcr.io/jozala/omnigrex/pi:sha-${COMMIT}"
docker image inspect "ghcr.io/jozala/omnigrex/${CONTROL_PLANE}:sha-${COMMIT}" --format '{{index .RepoDigests 0}}'
docker image inspect "ghcr.io/jozala/omnigrex/pi:sha-${COMMIT}" --format '{{index .RepoDigests 0}}'
```

The inspected values must match the workflow summary and are recorded as `OMNIGREX_ORCHESTRATOR_IMAGE` and `OMNIGREX_PI_RPC_V1_IMAGE` in the private `.env`.
The native backend also needs [exact registry provisioning](#microsandbox-bundle) into its own cache; a host Docker pull alone is insufficient.
Until a commit has both-backend publication and post-publication smoke evidence, source fixtures do not establish a deployable Pi release.

For legacy OpenCode only, resolve the two matching images:

```sh
docker pull --platform=linux/amd64 "ghcr.io/jozala/omnigrex/orchestrator:sha-${COMMIT}"
docker pull --platform=linux/amd64 "ghcr.io/jozala/omnigrex/opencode:sha-${COMMIT}"
docker image inspect "ghcr.io/jozala/omnigrex/orchestrator:sha-${COMMIT}" --format '{{index .RepoDigests 0}}'
docker image inspect "ghcr.io/jozala/omnigrex/opencode:sha-${COMMIT}" --format '{{index .RepoDigests 0}}'
```

The inspected `...@sha256:...` values must match the digests in the workflow summary.
Record them in `.env` as `OMNIGREX_ORCHESTRATOR_IMAGE` and `OMNIGREX_OPENCODE_ACP_V1_IMAGE`, keep `OMNIGREX_OPENCODE_ACP_V1_PLATFORM=linux/amd64`, and fill in the host-specific settings and file-backed secrets described below.
The deployment bundle uses one OpenCode digest for the `opencode-image` validation service, `OMNIGREX_AGENT_IMAGE_REFERENCE`, and `OMNIGREX_OPENCODE_ACP_V1_IMAGE` so they cannot diverge.

## GitHub Apps

Create two GitHub Apps under the account or organization that owns the managed repositories.
The App names are operator-selected, but the Apps must have different numeric IDs and different private keys.
User authorization, callback URLs, and OAuth features are not required.

### Developer App

Configure the Developer App with these exact repository permissions:

| Repository permission | Access |
| --- | --- |
| Metadata | Read-only |
| Checks | Read-only |
| Contents | Read and write |
| Issues | Read and write |
| Pull requests | Read and write |
| Workflows | Read and write |

Subscribe to exactly these repository events:

- Issues.
- Pull request.
- Pull request review.
- Repository.

The Repository subscription delivers `repository.created` so newly created
repositories are provisioned. The Developer App also receives the default
`installation.created` and `installation_repositories.added` deliveries.

Enable the webhook and configure:

| Setting | Value |
| --- | --- |
| URL | `https://PUBLIC_HOST/webhooks/github` |
| Content type | `application/json` |
| Secret | The exact contents of `secrets/github-webhook-secret` |
| SSL verification | Enabled |

The URL must use public HTTPS and exactly the `/webhooks/github` path without credentials, a query string, or a fragment.

### Reviewer App

Configure the Reviewer App with these exact repository permissions:

| Repository permission | Access |
| --- | --- |
| Metadata | Read-only |
| Contents | Read-only |
| Pull requests | Read and write |

Select no webhook events and disable the webhook.
The Reviewer App must not reuse the Developer App identity or private key.

### Agent Participant Signatures

GitHub attributes every comment to the GitHub App that published it, so
comments from different Agent Profiles sharing one App look identical.
Every new Agent Participant comment, Pull Request comment, native review
body (including otherwise bodyless approvals), and inline review comment
ends with a visible footer identifying its Agent Profile:

```text
_By Omnigrex: `profile-name` [Role Display Name]_
```

The profile name comes from the Agent Participant's validated Agent Turn
identity and the display name from the configured Role catalog. The hidden
idempotency marker follows the footer. Historical comments and Human
Handoff diagnostics are never re-signed.

### Keys And Installation

Generate and download one private key from each App's settings page.
Omnigrex accepts RSA private keys encoded as PKCS#1 or PKCS#8 PEM.
Store the files at:

```text
secrets/github-developer-private-key.pem
secrets/github-reviewer-private-key.pem
```

Set the numeric App IDs in `.env`:

```text
OMNIGREX_GITHUB_DEVELOPER_APP_ID=12345
OMNIGREX_GITHUB_REVIEWER_APP_ID=67890
```

Open each App's installation page, select the owning account or organization, and grant the App access to every repository Omnigrex will manage.
When using selected-repository installations, add each new repository to both installations before running preflight.
Omnigrex creates short-lived installation tokens, so no personal access token is required.

## Secrets

Generate independent database and webhook secrets:

```sh
openssl rand -base64 32 > secrets/database-password
openssl rand -hex 32 > secrets/github-webhook-secret
```

Copy the two GitHub App private keys into the paths described above.
Copy one Pi-native API-key bundle for a Pi deployment:

```sh
cp /secure/path/pi-auth.json secrets/provider-credentials.json
chmod 0640 secrets/*
```

The provider credential file must contain a nonempty JSON object with literal Pi-native API keys:

```json
{
  "provider-name": {
    "type": "api_key",
    "key": "REPLACE_WITH_SECRET"
  }
}
```

The requested `provider/model` must be available in the exact Pi image and usable with the selected provider key.
Executable key references, environment references, and OAuth credentials are unsupported.
Only the selected entry is written to mode-0600 `/run/pi-agent/auth.json` in disposable tmpfs; MCP bearer authority stays in memory.
Keys are excluded from normal retained-state, database, image, and diagnostic paths.
Repository code can deliberately read or copy supplied keys, including from a microVM; this is not credential mediation.
Preflight checks configuration and capabilities without making a paid provider request.
For the transitional OpenCode stack, keep its existing `type: api` bundle instead; do not share incompatible credential shapes across runtimes.

### Secret File Group

On Linux, read the Docker socket group ID:

```sh
stat -c '%g' /var/run/docker.sock
```

Set that value as `OMNIGREX_DOCKER_GID` in `.env`.
Use `OMNIGREX_DOCKER_GID=0` with Docker Desktop.

Create a dedicated host group for Omnigrex secret files using the host operating system's account-management tools.
Change `secrets/*` to that group and set its numeric group ID as `OMNIGREX_SECRET_GID`.
The secret group ID must differ from the selected backend's Docker socket or KVM group ID, and unrelated host users must not belong to the secret group.
Microsandbox uses the KVM group procedure in [Microsandbox bundle](#microsandbox-bundle) and does not require `OMNIGREX_DOCKER_GID`.

Docker socket group membership is equivalent to host-root authority.
Mounting the socket read-only prevents file writes through the mount but does not restrict Docker API operations.

## Agent Profiles

Every managed repository must contain one Agent Profile for every Workflow Role on its default branch before the first trigger.
Profiles are discovered as direct lowercase `*.md` children of:

```text
.omnigrex/team/
```

Filenames are arbitrary and do not define Profile identity or Role.
The current selector requires exactly one discovered Profile for each Workflow Role.
Additional Profiles for the same Role are rejected until multiple-Profile selection is configured.

Each file starts with strict YAML front matter followed by Role instructions.
The supported fields are:

| Field | Requirement |
| --- | --- |
| `name` | Repository-local stable identity using lowercase letters, digits, hyphens, or underscores |
| `role` | Role ID referenced by the active Workflow Definition, such as `DEVELOPER` or `REVIEWER` |
| `runtime` | `pi-rpc/v1` for Pi; `opencode-acp/v1` remains transitional |
| `model` | Provider model in `provider/model` form |
| `thinking` | Optional Pi level: `off`, `minimal`, `low`, `medium`, `high`, or `xhigh`; actual model support is verified |
| `permissions` | Nonempty map of local permissions to `allow` or `deny` |

Pi local permission names are `read`, `edit`, `write`, `bash`, `grep`, `find`, `ls`, and the instruction permission `skill`.
Every omitted permission defaults to deny.
The Reviewer cannot allow `edit` or `write`.
Pi rejects `steps`, `variant`, unavailable OpenCode tool names, and delegation permissions; there is no OpenCode-style step limiter.
The transitional OpenCode schema retains `variant`, integer `steps` from 1 through 1000, and its original tool vocabulary until Step 17.
Runtime-provided Omnigrex MCP tools are authorized separately from these local permissions.

A Pi Developer Profile can use:

```yaml
---
name: developer
role: DEVELOPER
runtime: pi-rpc/v1
model: openai/gpt-4.1
permissions:
  read: allow
  edit: allow
  write: allow
  bash: allow
  grep: allow
  find: allow
  ls: allow
  skill: allow
---
Load the repository's implementation skill and follow the Stage guidance.
```

Choose a model supported by the pinned image and actual credentials; this example is not a provider/model allowlist.
For Reviewer use `role: REVIEWER` and deny or omit `edit` and `write`.
Local `bash` runs trusted repository code and is not a security boundary against arbitrary file edits; Role and MCP authority remain separately enforced.

### Instruction Ownership and Operator Guidance

Omnigrex supplies platform instructions and the current Workflow Stage's objective, completion criteria, and purpose-specific guidance.
Repository Agent Profiles select skills and describe personality, communication style, and repository preferences.
Skills provide engineering procedures rather than owning platform permissions or Workflow transitions.

Instruction precedence is platform and Stage contracts, operator Role guidance, operator common guidance, and repository preferences, in that order.
This describes how guidance is interpreted; actual capabilities and transitions remain code-enforced.
Instructions from the running deployment supersede older deployment guidance in retained Agent Session history.

Optionally set `OMNIGREX_AGENT_INSTRUCTIONS_DIR` to a clean absolute path inside the orchestrator containing:

```text
agent-instructions/
  common.md
  roles/
    DEVELOPER.md
    REVIEWER.md
```

Use exact Role IDs from the deployment's Role Policy Catalog, not repository Profile names.
The common file and individual Role files are optional; an unset directory adds no operator guidance.
A configured missing or unreadable directory, unexpected entry, unknown Role, non-text file, invalid UTF-8, NUL byte, or file exceeding 64 KiB fails startup and the operator-instruction preflight check.
The common and applicable Role instructions must also fit a combined 64 KiB JSON-encoded budget, including escaped characters.
Pi configuration travels over stdin rather than an environment entry; the Pi adapter validates its bounded configuration and verified effective model/thinking before prompt admission.
The transitional OpenCode adapter additionally enforces its 120 KiB composed environment-configuration limit.
Empty instruction files add no guidance.

Create a Compose override alongside the deployment files, for example `compose.instructions.yaml`:

```yaml
services:
  orchestrator:
    environment:
      OMNIGREX_AGENT_INSTRUCTIONS_DIR: /etc/omnigrex/instructions
    volumes:
      - type: bind
        source: ./agent-instructions
        target: /etc/omnigrex/instructions
        read_only: true
        bind:
          create_host_path: false
```

Create the host directory and make its files readable by the orchestrator, then append `:compose.instructions.yaml` to the already selected `COMPOSE_FILE` once before starting the deployment.
Keep the common bundle, selected backend, storage overrides, and instruction override in that same exported file set.
The directory is mounted only into the orchestrator; Runtime Processes receive composed text, not the files or host paths.
The standard Compose bundles also forward `OMNIGREX_AGENT_INSTRUCTIONS_DIR` if set through the deployment environment, but the operator must provide the corresponding mount.

Files are loaded once at orchestrator startup.
After editing them, recreate the orchestrator with the same Compose file set, for example:

```sh
docker compose up -d --force-recreate orchestrator
```

New launches, including Session Continuation and recovery, use the running binary's core/Stage guidance and its startup-loaded operator instructions.
There is no persisted composed-instruction snapshot to replay after an upgrade and no hot reload inside an executing Agent Turn.
Repository Profile provenance remains separate and follows the default-branch rules below.
See [decision 0002](./decisions/0002-use-current-deployment-agent-instructions.md) and [decision 0003](./decisions/0003-separate-personality-from-platform-and-workflow-guidance.md).

### Repository Skills

Both Roles can use explicitly authorized checkout-local skills when their Agent Profile includes `skill: allow` and `read: allow` under `permissions`.
The Pi owned runner discovers physical `.agents/skills/<name>/SKILL.md` files in the prepared checkout, advertises their descriptions, and uses Pi `read` to load their bodies.
Symlink-expanded discovery is rejected; uncontrolled project extensions, settings, packages, prompt templates, and automatic context/skill discovery remain disabled.
This adds a repository skill source without enabling sub-agents or changing MCP authority.
Profiles without skill permission do not receive this explicit source.

Store each skill in `.agents/skills/<name>/SKILL.md` with front matter such as:

```yaml
---
name: repository-example
description: Explain the procedure and when the agent should load it.
---
```

Use a unique name matching the directory and put the procedure after the front matter.
The agent sees the skill description and can load its body on demand with the permitted runtime reading tool.
Put optional examples and references in the same skill directory, with explicit instructions about when to read them.
Keep personality, communication preferences, and skill selection in the Agent Profile; platform and Stage instructions supply operational rules and approval criteria.

Skills come from the prepared working checkout, not the default-branch snapshot used for Agent Profiles.
The Developer may initially use default-branch skills and later use Change Proposal skills; the Reviewer uses skills from its Change Proposal checkout.
New Runtime Processes rediscover skills when continuing retained Agent Sessions, but in-process edits are not guaranteed to hot-reload.
Omnigrex's repository Profiles require loading their Role skill on each applicable Agent Turn, and platform instructions require reporting a blocker when required guidance is unavailable.
Publish those Profiles and their required skills together so the next Turn can find them.
For an existing Change Proposal, ensure its checkout contains any newly required skills before starting or resuming work.

A Change Proposal can modify skill instructions used in its own review.
The separate [human approval gate for skill changes](https://github.com/jozala/omnigrex/issues/43) is planned, not currently enforced.
See [ADR 0012](./adr/0012-load-repository-skills-from-the-current-checkout.md) and [Pi resource ownership](./decisions/0014-own-pi-resource-loading-with-a-pinned-runner.md).

### Git Identity and Review Handoffs

Workspace preparation sets repository-local `user.name=Omnigrex` and `user.email=agent@omnigrex.invalid` before the agent starts.
This fixed identity supports normal and merge commits without agent-side setup and does not change existing commits or global Git configuration.

Each new `request_review` call publishes its summary as a signed PR comment identifying the exact head before its terminal handoff can succeed.
This applies to the initial PR and subsequent requested-changes Turns; agents should not post a duplicate summary or call `open_pr` for an existing PR.
The next Stage waits for confirmed publication.
An uncertain response is reconciled against the exact reserved marker, summary, and signature instead of blindly posting again.
Existing bounded mutation recovery applies, with Human Handoff when uncertainty cannot be resolved.

Both Roles can use `get_handoff` to retrieve the latest successful summary for their scoped Workflow, PR, and head.
The response contains the durable handoff ID, head, summary, creation time, and `publication_confirmed`; it is `null` when no handoff matches.
Historical internal-only handoffs remain readable with `publication_confirmed=false`; they are not retroactively described as published comments.
Already completed historical intents retain their recovery compatibility, while new requests use the publication barrier.
The Reviewer treats the summary as a claim to verify against code and test evidence.
Native inline findings are submitted through `submit_review.comments`, with the review body reserved for findings without valid diff locations.

### Agent Turn Tool Paths

An optional `.omnigrex/turn-configuration.yaml` on the default branch requests disk-backed directories for tool environment variables.
Omnigrex reads it from the same pinned default-branch commit as the Agent Profiles for each Agent Turn and applies it to both Roles.
The file is strictly validated; unknown fields, unsupported sizes, unapproved variables, and invalid directories cause a configuration Human Handoff before the Runtime Process starts.
For example:

```yaml
version: 1
environment-paths:
  directories:
    build-tmp:
      lifecycle: turn
    go-build-cache:
      lifecycle: assignment
  environment:
    - name: TMPDIR
      directory: build-tmp
    - name: GOTMPDIR
      directory: build-tmp
    - name: GOCACHE
      directory: go-build-cache
```

Set `OMNIGREX_AGENT_PATH_ENV_ALLOWLIST` to a comma-separated list of approved variable names in the deployment configuration, such as `TMPDIR,GOTMPDIR,GOCACHE,GOPATH`.
The operator allowlist is shared across Roles; the repository may request only approved names and cannot supply path values.
Omnigrex always rejects runtime-control and credential-related names such as `PI_*`, `OPENCODE_*`, `OMNIGREX_*`, `NODE_OPTIONS`, `HOME`, and `PATH` even if listed by the operator.
Without the optional file, `TMPDIR` uses the Runtime Profile's bounded temporary path, `/tmp` for Pi and `/tmp/opencode` for transitional OpenCode.
Requested paths are created on the disk-backed mise volume under an isolated Assignment subpath, outside the repository workspace and Change Proposal tree.
Turn-lifecycle scratch is removed after the Runtime Process stops; assignment-lifecycle caches remain across turns and are removed during Assignment collection.
These directories have no per-Participant disk quota, like the workspace storage, so monitor available backing storage on the selected backend.
A background monitor measures only the Agent Participant assignment-lifecycle cache area (on disk under the historical `assignment-<ID>` prefix) every `OMNIGREX_ASSIGNMENT_TOOL_CACHE_POLL_INTERVAL` (default `5m`) and logs a soft warning when allocated disk usage reaches `OMNIGREX_ASSIGNMENT_TOOL_CACHE_WARNING_MIB` (default `1024` MiB).
The warning carries only `agent_participant_id`, `size_bytes`, and `threshold_bytes`, is emitted once per unchanged size, never blocks an Agent Turn, and never changes retention or cleanup behavior.
Measurement is read-only, counts allocated filesystem blocks including directories, and resumes retained directory streams across bounded scan slices without replaying entries or changing modes.
The monitor retains at most four active scans, each with at most 32 directory handles and 8192 hardlink identities; ordinary files require no retained identity set.
Collection, cache replacement, fatal errors, completion, and shutdown release retained handles.
Unreadable or unsafe entries and depth or hardlink limits produce partial (truncated) measurements, which never clear a warning based on a below-threshold partial sum.
Moving build output to disk does not increase the Execution Environment's memory limit or constrain compiler parallelism.
This implementation changes the `opencode-acp/v1` Runtime Profile content hash; existing Agent Sessions bound to the earlier contract cannot continue unless the deployment resets their stored associations before upgrading.

Agent Profiles are loaded from the latest default-branch commit before each Agent Turn.
Directory listing and every Profile file are read from the same exact commit.
Every direct Markdown file is treated as configuration; malformed files and unknown or unreferenced Roles fail discovery.
A file can move without changing Profile identity when its front matter name remains unchanged.
A feature branch cannot replace the Reviewer Profile used to review that branch.
Changing the Runtime Profile reference of an existing Agent Participant does not migrate its Agent Session and creates a configuration Human Handoff.

Never place provider credentials, GitHub credentials, deployment secrets, or private endpoint credentials in an Agent Profile.

## Runtime Profile Image

A Pi no-clone deployment uses the published Pi golden digest for the selected qualified commit.
Set the digest from the CI workflow summary and matching platform in `.env`:

```text
OMNIGREX_PI_RPC_V1_IMAGE=ghcr.io/jozala/omnigrex/pi@sha256:...
OMNIGREX_PI_RPC_V1_PLATFORM=linux/amd64
```

Only `linux/amd64` is published initially.
The exact digest must already be available to the selected backend because Omnigrex does not replace unavailable images with mutable tags.
Pull it explicitly into Docker or the native cache before startup (see [Pi operational deployment](#pi-operational-deployment)).
Keep every digest referenced by active or retained Agent Sessions available until the corresponding state is garbage-collected.
The same requirement applies when restoring a backup on another host: restore `.env`, secrets, compatibility artifacts, the deployed commit, and every referenced image digest before changing current data.

### Repository Environments

Optionally publish `.omnigrex/environment.yaml` on the repository's default branch:

```yaml
version: 1
image: registry.example/team/development@sha256:<64-lowercase-hex-digest>
platform: linux/amd64
requirements:
  - process-isolation
  - tool-installation
```

Replace the example digest with the resolved registry digest of a prebuilt image derived from the current Omnigrex golden image.
Without this file, new Participants select the deployment golden image for their Agent Profile's runtime, Pi after cutover.
Each Participant retains its selected exact image, platform, and full versioned contract; later Participants can select newer images even in the same Assignment Generation.
Retained Participants do not rebind when this file or deployment configuration changes.
Pre-Step07 Participants continue inside their original images using a bootstrap staged from the configured deployment golden image into read-only-mounted Turn scratch.
Keep a Step07-capable deployment golden image available for their platform as well as every retained Participant image; an unavailable bootstrap source fails rather than skipping repository tool setup.
Keep their images available until Session collection, and include PostgreSQL's immutable contract records in the coordinated recovery set.

Build Pi repository images outside Omnigrex using the [Debian-based Pi example Dockerfile](../agent/environments/pi-example/Dockerfile):

```sh
docker build --platform linux/amd64 --file agent/environments/pi-example/Dockerfile \
  --build-arg OMNIGREX_GOLDEN_IMAGE=ghcr.io/jozala/omnigrex/pi@sha256:<golden-digest> \
  --build-arg EXTRA_DEB_PACKAGE='jq=<approved-exact-Debian-version>' \
  --tag registry.example/team/development:qualified .
docker push registry.example/team/development:qualified
docker image inspect registry.example/team/development:qualified --format '{{index .RepoDigests 0}}'
```

Use real approved digests and package versions instead of the placeholders, then put the resulting untagged `registry.example/team/development@sha256:...` reference in the environment file.
Preserve Node 24.14.1, Pi 1.0.4 and its locked package/resource inventory, the Omnigrex-owned runner/extensions/bootstrap, mise, Git/shell/search resources, and UID/GID 10001.
The example installs an exact Debian package version; use a controlled package mirror/snapshot when reproducible rebuilding is required.
For the transitional OpenCode golden image, the [Alpine example](../agent/environments/example/Dockerfile) still applies; its `apk` commands do not work on the Pi base.
Place additional image-provided tools under `/usr/local/bin` or activate them explicitly through working-revision mise configuration.
Image labels and ancestry do not replace executable/resource qualification.
Omnigrex does not build repository images; provision the exact digest/platform into the selected backend before accepting work.
Run the existing repository `doctor` command to exercise golden-resource qualification and capability validation before adding the trigger label.
Doctor verifies immutable golden resources and requirements, not the repository's entire development test suite.
Qualify additional tools and the repository's actual build/tests in an isolated Work Item using the selected backend before promoting its environment file.
For later Pi source-to-candidate continuation, use [Pi upgrade qualification](#pi-upgrade-and-state-recovery-qualification) on copies of source state; a new independent Participant image does not imply or authorize rebinding retained Sessions.

Docker accepts `process-isolation` and `tool-installation` requirements and explicitly rejects `hardware-isolation` and `nested-containers`.
Microsandbox additionally accepts `hardware-isolation` and `nested-containers` under its qualified Linux/KVM contract; Omnigrex never mounts the host Docker socket into a guest as a fallback.
Unknown requirements, mutable image tags, unsupported platforms, and unknown YAML fields fail configuration.

Image selection and Agent Profiles use the same trusted default-branch snapshot.
Tool installation uses the actual working revision for both Developer and Reviewer, including the Change Proposal's mise hooks and scripts.
This keeps default-branch Profile authority while allowing the Reviewer to test the toolchain declared by the code it reviews.
Installation occurs in the managed non-root environment before agent-protocol initialization, using disk-backed installation scratch and a separate tool-data attachment for requested caches.
Repository activation cannot replace protected runtime settings, provider configuration, or loader injection variables, and the agent executable is launched by its absolute qualified path.
Approved control-plane directory bindings such as `GOCACHE` and `GOPATH` take precedence over conflicting mise activation and retain their configured storage and cleanup lifecycle.
The existing trusted-repository credential model still applies to repository scripts and development images.
See the [implementation and qualification evidence](./plans/2026-10-08-repository-environments.md).

### Agent Turn Memory

Set `OMNIGREX_AGENT_TURN_MEMORY_MIB` in `.env` to a positive integer number of MiB per Agent Turn Execution Environment.
The Pi bundle defaults to `1024` MiB (1 GiB); the legacy bundle defaults to `512`.
After changing `.env`, recreate the orchestrator with `docker compose up -d --force-recreate orchestrator` and run `doctor` for each repository.
New Agent Turns and runtime diagnostic probes use the new limit; running environments are not resized.
Changing this setting does not change the Runtime Profile binding or require a database reset.
At the default concurrency of two, 1 GiB per Turn allows up to 2 GiB across Execution Environments, in addition to native VMM overhead and other host processes.
For the historical OpenCode deployment only, the first memory-independent `opencode-acp/v1` contract changed its content hash once.
Coordinate the agreed one-time database reset with active turns before that upgrade; it discards Omnigrex Workflow and session associations, including those for existing Issues and Pull Requests, while GitHub Issues, Pull Requests, and labels remain.
Later memory-setting changes do not require another reset.

## HTTPS Ingress

The Compose stack publishes its HTTP server on `${OMNIGREX_HTTP_PORT:-8080}` and does not publish the private MCP listener.
When the reverse proxy runs on the same host, bind the backend to loopback by setting:

```text
OMNIGREX_HTTP_PORT=127.0.0.1:8080
```

The reverse proxy must:

- Terminate TLS with a publicly trusted certificate.
- Forward `POST /webhooks/github` to `http://127.0.0.1:8080/webhooks/github` without changing the path.
- Preserve the exact raw request body.
- Preserve `X-Hub-Signature-256`, `X-GitHub-Delivery`, and `X-GitHub-Event`.
- Allow request bodies up to 25 MiB.
- Complete request headers within five seconds and the request within fifteen seconds.
- Keep port 8081 and the `omnigrex-agent` network private.

A minimal host-level Caddy configuration is:

```caddyfile
omnigrex.example.com {
  handle /webhooks/github {
    reverse_proxy 127.0.0.1:8080
  }

  respond 404
}
```

Restrict direct access to the Compose HTTP port with a host firewall when it cannot be bound to loopback.
Omnigrex does not consume forwarded-client headers and does not require the proxy to supply them.

After deployment, use the Developer App's Advanced page to inspect a recent delivery or request a redelivery.
A valid delivery must receive HTTP `202 Accepted`.
This GitHub-side delivery check is required because `doctor` cannot prove that the remote webhook secret equals the local secret or that public DNS, TLS, firewall, and proxy routing work end to end.

## Deploy

For Pi, use the [selected backend bundle](#pi-operational-deployment), pre-provision exact images, validate Compose with `docker compose config --quiet`, and start with `docker compose up --no-build --wait`.
For the transitional OpenCode deployment only, use the pinned `compose.yaml`, `.env`, and operator-provided `secrets/`, pre-pull its exact digest, validate Compose, and start without `--build`.
Docker Compose loads `.env` automatically, but the shell does not, so source it first for the explicit pre-pull:

```sh
set -a
. ./.env
set +a
docker pull --platform=linux/amd64 "$OMNIGREX_OPENCODE_ACP_V1_IMAGE"
docker compose config --quiet
docker compose up --wait
```

Verify local health and dependency readiness:

```sh
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
```

Run preflight once for every managed repository:

```sh
docker compose exec orchestrator \
  /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

Every line must report `PASS`, and the command must exit with status 0.
Configuration failures are reported together before external checks begin.
Independent external checks continue after failures so the operator receives one diagnostic set.

The PostgreSQL check uses a read-only connection and does not apply migrations.
Docker resource inspection does not write named volumes.
The configured runtime check creates and removes one temporary Runtime Process with tmpfs-backed Session and credential paths.
GitHub installation checks create short-lived tokens but do not modify repository content or start a Workflow.

Finally, verify a real GitHub webhook delivery from the Developer App.
Do not add `omnigrex:run` until preflight and webhook delivery verification both pass.

## Automatic Label Provisioning

When the Developer App gains access to a repository, Omnigrex queues provisioning of the five managed labels using only Developer App credentials:
Provisioning is asynchronous; wait until the managed labels, including `omnigrex:run`, appear before adding the trigger label to an Issue.

- A new Developer App installation provisions every accessible repository.
  Accessible repositories are enumerated through a paginated
  installation-token request, not the webhook repository list.
  Enumeration is bounded at 100 pages of 100 repositories; an installation
  with more than 10,000 accessible repositories fails observably for
  operator follow-up instead of provisioning partially.
- Explicitly added repositories are provisioned from the
  `installation_repositories.added` delivery.
- Repositories created under all-repository access are provisioned from the
  `repository.created` delivery.

Onboarding deliveries use a separate processor, so installation enumeration does not block Issue and Pull Request webhook processing.
Provisioning creates missing label names only, preserves the color and
description of existing labels, leaves unrelated labels untouched, and never
applies a state label to an Issue or Pull Request or starts a Workflow.
Duplicate or overlapping deliveries are harmless, one repository's failure
does not block other repositories, and terminal failures are recorded as
observable provisioning and delivery failures for operator follow-up.

Inspect webhook deliveries and their repository jobs together when provisioning fails.
A delivery in state `FAILED` with a `last_error` may have queued no jobs because its payload or installation enumeration failed, but valid entries in a mixed delivery or earlier committed batches can leave jobs even when a later attempt exhausts the delivery.
Look for `kind = 'PROVISION_MANAGED_LABELS'` rows in `jobs` for that delivery; each job's `idempotency_key` has the form `label-provisioning:<delivery-id>:<repository-id>` and its `payload` identifies the repository.
A delivery with only valid entries becomes `PROCESSED` after its jobs are queued; a later terminal failure of one repository appears on its `jobs` row with status `FAILED` and a `last_error`.
Provisioning jobs leave `normalized_event_id` empty because onboarding deliveries do not create normalized Workflow events.

Repositories installed before this provisioning was deployed are not backfilled on startup.
Their missing labels are created by the existing Workflow label reconciliation the next time a Workflow runs.
Labels deleted from an idle repository are likewise restored only when a Workflow runs.

Onboarding scale and webhook admission are bounded.
The ingress accepts webhook bodies up to 25 MiB, covering GitHub's 25 MB payload cap; larger bodies receive HTTP `413` before authentication or durable recording.
GitHub does not deliver payloads exceeding its own cap, so no webhook-triggered provisioning occurs in that case either.
Large installations within these limits are queued in bounded batches with claim renewal between batches; progress is observable as `PROVISION_MANAGED_LABELS` job rows.
Transient failures while converting a provisioning webhook into durable work defer the next delivery attempt with increasing delays and GitHub rate-limit timing; these deliveries allow up to eight attempts.
If onboarding was not delivered or an installation exceeds the 100-page enumeration limit, the operator must create the missing `omnigrex:run` repository label before it can be applied to an Issue.
The same manual trigger-label step applies to repositories installed before this feature when that label is absent.
After the first Workflow starts, existing Workflow label reconciliation creates any other missing managed labels.

## Start A Workflow

Create a small Issue with explicit acceptance criteria and add the `omnigrex:run` label.
Omnigrex creates missing workflow labels when needed.

| Label | Meaning |
| --- | --- |
| `omnigrex:run` | Human command to create or reactivate a Workflow Attempt |
| `omnigrex:developing` | Developer work is active or pending |
| `omnigrex:reviewing` | Reviewer work is active or pending |
| `omnigrex:pr-ready` | The current Pull Request head is approved and ready for human review |
| `omnigrex:needs-human` | Autonomous progress stopped in a Human Handoff |

GitHub labels are the human-visible state, while PostgreSQL is the operational ledger.
Do not edit PostgreSQL records to force a transition.

## Retry And Review Budgets

An Agent Turn with no eligible successful terminal intent receives one infrastructure retry as a new Agent Turn in the same Agent Session.
If a unique terminal mutation intent succeeded before a runtime response was lost or the prompt deadline elapsed, Omnigrex can settle the Turn successfully after corroborating the current Pull Request, review, or workspace state.
For a successful Developer or Reviewer outcome, the prompt failure remains in the settlement diagnostic; a turn without a successful terminal intent still follows the infrastructure retry policy.
Explicit cancellation and ineligible runtime terminal outcomes do not take this successful settlement path.
Infrastructure retries do not consume the review budget.
After the second failure, Omnigrex publishes diagnostics and creates a Human Handoff.

When `request_review` or `submit_review` succeeded and the runtime ending is eligible but a fresh corroboration read is unavailable, Omnigrex stops the Runtime Process and keeps the original Turn fenced while a separate worker retries the read.
It does not prompt the agent or repeat the GitHub mutation.
The default window is 30 minutes, configured by `OMNIGREX_TERMINAL_CORROBORATION_DURATION`.
Its start is durable across orchestrator restarts; changing the configured duration changes the remaining window at the next check.
The Issue retains its current developing/reviewing label while verification is pending.
A clear access prerequisite or exhausted window creates a Human Handoff explaining that the mutation succeeded but the outcome was not accepted.
Closing the Issue cancels pending verification.
If Runtime Process cleanup or the orchestrator fails after the runtime ending was atomically recorded with closed mutation admission, recovery first proves the process stopped and settles the mutation ledger before creating the original Turn's pending verifier.

After that specific Human Handoff, a new human `omnigrex:run` first revalidates the old intent without another agent prompt **when** the Stage and Role still match, the Assignment Generation has not been replaced, and the Agent Session is automation-controlled.
The old failed settlement is not rewritten; a fresh read must still prove the current PR/review identity and Developer workspace tree.
If the Session is human-controlled, the trigger remains in Human Handoff rather than starting an unpreparable Turn.
If the Stage or Assignment Generation changed, normal reactivation applies instead of adopting the old intent.
If the old runtime ending was **not** durably known because execution crashed earlier, ordinary infrastructure recovery gives the successor Agent Turn an explicit prior-intent notice.
That agent can call `confirm_prior_terminal_intent` for the recorded source mutation; Omnigrex verifies it without submitting a duplicate review.
A review submitted before a **known explicit cancellation** cannot be confirmed or resubmitted in the retry; the successor is instructed to report the blocker for Human Handoff.
Mere Agent Session memory or a final text response is not a terminal intent.

Each Workflow Attempt permits at most three accepted Review Cycles.
A review counts only after the Reviewer App publishes it for the expected Pull Request head and that head remains current.
A stale review does not consume the budget.
The third accepted changes-requesting review creates a Human Handoff.

Re-adding `omnigrex:run` creates a new Workflow Attempt, resets both budgets, and reuses retained Agent Sessions when possible.
The default Agent Turn timeout is two hours, and the default global Agent Turn concurrency is two.

## Session Retention

Closing an Issue fences new work, settles admitted mutations, completes its current Agent Participants, and schedules Runtime Process state deletion.
The default retention period is 30 days and is configured with `OMNIGREX_ASSIGNMENT_RETENTION_DURATION`.
Reopening before garbage collection cancels scheduled deletion but does not restart automation.
Add `omnigrex:run` after reopening to create a new Workflow Attempt.

Once physical collection starts, deletion is intentionally irrevocable.
After collection, a new trigger creates a new Assignment Generation whose Stage Assignments lazily select new Agent Participants and Agent Sessions, while PostgreSQL retains prior operational history.

Every active or retained Agent Session stays pinned to its original Runtime Profile image digest.
Keep every referenced digest available until the corresponding state is garbage-collected.
Retained Runtime State also has an immutable Backend family, contract version, and storage namespace binding.
The existing Docker deployment claims its configured runtime-state volume namespace once after migration 000032; changing that configuration later fails instead of attaching different data.
An initialized Participant state directory that disappears fails availability and launch checks instead of starting an empty conversation store.
Restore the matching state before resuming work.

## Backup

PostgreSQL and the runtime-state volume form one coordinated recovery set, plus the Microsandbox protected journal when that backend is selected.
PostgreSQL is the authoritative execution ledger, including canonical Runtime Profile contracts and immutable backend/image ownership; the runtime-state volume contains Pi Session data needed for Session Continuation and History Replay (or transitional OpenCode state before reset).
The same volume now contains sealed Workspace Captures under `workspace-captures`, mounted only into the orchestrator at `/var/lib/omnigrex/runtime-state/workspace-captures`.
These captures preserve original-Turn publication and terminal evidence through deferred corroboration and human revalidation, even after its Execution Environment and live workspace have been removed.
They are protected retained data, collected with their Agent Participant rather than with Turn scratch or the workspace cache.
The standard stopped-orchestrator runtime-state archive below includes them automatically; selective Session-only exports are not a complete recovery set.
See [the workspace evidence contract](./plans/2026-10-08-location-independent-workspaces.md) for capture format and limits.
Do not copy runtime state while an Agent Turn is active.

Stop accepting work at the ingress and stop the orchestrator gracefully, using the same selected Compose file set and a grace period long enough for the configured cleanup budgets.
Verify that no managed Runtime Process remains:

```sh
docker compose stop -t 60 orchestrator
docker ps --filter label=io.omnigrex.runtime-process=true --format '{{.ID}} {{.Names}}'
```

The second command must produce no output before backup continues.
If a process remains, do not copy state; investigate and let unchanged-configuration startup recovery resolve exact managed executions, then stop again.
For Microsandbox, also inspect native inventory while the orchestrator service is stopped:

```sh
docker compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator list --quiet
```

Require no native resources before continuing this offline procedure; a stopped control-plane container alone does not prove that guest execution or an ambiguous dispatch is resolved.
Do not discard started environments or journals to bypass missing stop/writeback evidence.
Keep the original host fenced off throughout a compatible-host restore, including ingress and any human-prompt access.

Create a unique private recovery-set directory, then write each artifact atomically so a failed backup cannot replace a known-good recovery set:

```sh
(
  set -eu
  umask 077
  BACKUP_ROOT="$PWD/backups"
  BACKUP_DIR="$BACKUP_ROOT/$(date -u +%Y%m%dT%H%M%SZ)"
  install -m 0700 -d "$BACKUP_ROOT"
  mkdir -m 0700 "$BACKUP_DIR"
  cleanup_failed_backup() { rm -rf -- "$BACKUP_DIR"; }
  trap cleanup_failed_backup EXIT
  trap 'exit 1' HUP INT TERM

  docker compose exec -T postgres \
    pg_dump --username=omnigrex --dbname=omnigrex \
    --format=custom --no-owner --no-acl \
    > "$BACKUP_DIR/omnigrex.dump.tmp"
  docker compose exec -T postgres \
    pg_restore --list < "$BACKUP_DIR/omnigrex.dump.tmp" > /dev/null
  mv "$BACKUP_DIR/omnigrex.dump.tmp" "$BACKUP_DIR/omnigrex.dump"

  : "${BACKUP_DIR:?BACKUP_DIR must be set}"
  test -d "$BACKUP_DIR"
  HELPER='alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b'
  STATE_VOLUME=omnigrex-runtime-state # replace with this installation's exact volume name
  docker run --rm --network none --read-only \
    --mount "type=volume,src=$STATE_VOLUME,dst=/source,readonly" \
    "$HELPER" sh -ec 'cd /source; tar -czf - .' \
    > "$BACKUP_DIR/runtime-state.tar.gz.tmp"
  tar -tzf "$BACKUP_DIR/runtime-state.tar.gz.tmp" > /dev/null
  mv "$BACKUP_DIR/runtime-state.tar.gz.tmp" "$BACKUP_DIR/runtime-state.tar.gz"

  # For Microsandbox only: preserve the private dispatch journal, not VM images.
  # Set BACKEND=microsandbox in this shell; otherwise set BACKEND=docker.
  : "${BACKEND:?set the selected deployment backend}"
  if [ "$BACKEND" = microsandbox ]; then
    NATIVE_VOLUME=omnigrex-microsandbox # replace with this installation's exact native volume
    TURN_COUNT="$(docker compose exec -T postgres psql --username=omnigrex --dbname=omnigrex -Atc 'SELECT count(*) FROM agent_turns')"
    docker run --rm --network none --read-only \
      --mount "type=volume,src=$NATIVE_VOLUME,dst=/source,readonly" \
      --env "TURN_COUNT=$TURN_COUNT" \
      "$HELPER" sh -ec 'cd /source; if test -d omnigrex-operations; then tar -czf - omnigrex-operations; else test "$TURN_COUNT" = 0; dd if=/dev/zero bs=1024 count=1 2>/dev/null | gzip; fi' \
      > "$BACKUP_DIR/microsandbox-journal.tar.gz.tmp"
    tar -tzf "$BACKUP_DIR/microsandbox-journal.tar.gz.tmp" > /dev/null
    mv "$BACKUP_DIR/microsandbox-journal.tar.gz.tmp" "$BACKUP_DIR/microsandbox-journal.tar.gz"
  elif [ "$BACKEND" != docker ]; then
    exit 1
  fi

  (
    cd "$BACKUP_DIR"
    shasum -a 256 omnigrex.dump *.tar.gz > SHA256SUMS.tmp
    mv SHA256SUMS.tmp SHA256SUMS
  )

  trap - EXIT HUP INT TERM
  printf 'Recovery set: %s\n' "$BACKUP_DIR"
)
```

The archive preserves numeric ownership, including Runtime Process UID and GID 10001.
Migration 000032's Backend and versioned state metadata are part of the PostgreSQL dump and must be restored with the matching state volume.
The workspace and mise volumes are reproducible caches and are not required for disaster recovery when no Agent Turn is active.
The native image cache is re-provisioned from exact registry artifacts; disposable VM root disks, memory snapshots, guest tmpfs credentials, and tool scratch are excluded.
A diagnostic-only native installation may have no dispatch journal; an empty journal archive is allowed only when the stopped ledger has no Agent Turns.
If any Agent Turn exists and its journal is absent, backup fails rather than silently omitting potentially required recovery metadata.
Archive them separately if local policy requires a complete host checkpoint.

Back up `.env`, secret files, compatibility artifacts, the deployed Git commit, and every referenced image digest separately in encrypted storage.
Never place those files in the same unencrypted backup directory as operational exports.

Restart the unchanged deployment after the backup:

```sh
docker compose up -d --wait orchestrator
```

Run preflight before reopening ingress and human-trigger admission.

### Supplemental Session Exports

Use `omnigrex state export --participant UUID` for a checksummed, ownership-bound archive of one Agent Participant's retained Session data, including preparatory Pi Participants.
Use the exact Participant UUID from the operational ledger, not a Role, Agent Profile name, or runtime Session ID.
This export is supplemental: it does not replace the coordinated recovery set above because it excludes PostgreSQL and protected Workspace Captures.
Stop the orchestrator and confirm managed Runtime Processes are stopped as described above before running it.
The command also rejects active Turns, human prompt leases, irreversible collection, unresolved execution creation, and managed environments without confirmed stop evidence.
Each command has a ten-minute deadline, and cancellation interrupts blocked archive pipes as well as database operations.

Run the offline command using the same Compose file set and mounted runtime-state volume as the deployment, and publish its output only after successful completion:

```sh
(
  set -eu
  umask 077
  PARTICIPANT='<canonical-Agent-Participant-UUID>'
  ARCHIVE="session-${PARTICIPANT}.tar"
  trap 'rm -f -- "$ARCHIVE.tmp"' EXIT HUP INT TERM
  docker compose run --rm --no-deps -T --entrypoint /usr/local/bin/omnigrex \
    orchestrator state export --participant "$PARTICIPANT" > "$ARCHIVE.tmp"
  mv "$ARCHIVE.tmp" "$ARCHIVE"
  trap - EXIT HUP INT TERM
)
```

The archive carries a versioned manifest, exact Backend and Runtime Profile bindings, file inventory and permissions, and per-file SHA-256 checksums.
It rejects links, special files, known credential files, partial data, and unsupported formats rather than silently omitting them.
Maximum file content is 256 MiB, maximum encoded archive is 384 MiB, and maximum inventory is 100,000 entries.
Keep exports private as with the recovery set.

After restoring the matching PostgreSQL ledger and Backend namespace on a compatible host, restore a supplemental archive only into an absent Participant state resource:

```sh
PARTICIPANT='<canonical-Agent-Participant-UUID>'
docker compose run --rm --no-deps -T --entrypoint /usr/local/bin/omnigrex \
  orchestrator state restore --participant "$PARTICIPANT" < "session-${PARTICIPANT}.tar"
```

Restore validates the complete artifact in private staging before atomic publication.
It rejects a different Participant, Backend, namespace, state format, Runtime Profile, or image binding.
Retrying the same completed restore succeeds; different existing state is never overwritten or merged.
These commands do not convert conversations, remap storage namespaces, or authorize cross-backend migration.
Restart the unchanged deployment and run preflight as in the coordinated restore procedure below.

## Restore

Restore only from a PostgreSQL dump, runtime-state archive, and applicable native journal created in the same stopped-orchestrator backup window.
The following procedure is destructive and replaces current operational and session state.

Restore the matching `.env`, secrets, application revision, compatibility artifacts, and exact Runtime Profile images before changing current data.
The database password secret must match the restored PostgreSQL role because `POSTGRES_PASSWORD_FILE` does not alter an existing role password.
Use a compatible host with the same guest platform, Backend family/contract, namespace, and protected image/resource contracts.
Keep both hosts fenced until replacement readiness and preflight pass; never run two writers against the recovery set.
For Microsandbox re-provision all exact protected images into the replacement native cache before startup and restore its journal as shown below.

Select the recovery-set directory once, verify it, stop the complete stack without deleting volumes, and restore both state stores in one fail-fast operation:

```sh
(
  set -eu
  BACKUP_DIR='/absolute/path/to/backups/20260907T120000Z'
  HELPER='alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b'
  case "${BACKEND:?set docker or microsandbox as in the backup shell}" in
    docker|microsandbox) ;;
    *) exit 1 ;;
  esac
  if [ "$BACKEND" = microsandbox ]; then
    test -f "$BACKUP_DIR/microsandbox-journal.tar.gz"
  fi

  (
    cd "$BACKUP_DIR"
    shasum -a 256 -c SHA256SUMS
  )
  docker compose stop -t 60 orchestrator

  RUNTIME_PROCESS_IDS="$(docker ps --quiet --filter label=io.omnigrex.runtime-process=true)"
  if [ -n "$RUNTIME_PROCESS_IDS" ]; then
    printf 'Managed Runtime Processes remain after shutdown:\n%s\n' "$RUNTIME_PROCESS_IDS" >&2
    exit 1
  fi

  if [ "$BACKEND" = microsandbox ]; then
    NATIVE_RESOURCES="$(docker compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator list --quiet)"
    if [ -n "$NATIVE_RESOURCES" ]; then
      printf 'Native resources remain after shutdown; resolve recovery before replacing state.\n' >&2
      exit 1
    fi
  fi
  docker compose down

  docker volume create omnigrex-runtime-state
  docker run --rm --network none --read-only \
    --mount type=volume,src=omnigrex-runtime-state,dst=/target \
    --mount "type=bind,src=$BACKUP_DIR,dst=/backup,readonly" \
    "$HELPER" sh -ec \
    'find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -xzf /backup/runtime-state.tar.gz -C /target'

  if [ "$BACKEND" = microsandbox ]; then
    docker volume create omnigrex-microsandbox
    docker run --rm --network none --read-only \
      --mount type=volume,src=omnigrex-microsandbox,dst=/target \
      --mount "type=bind,src=$BACKUP_DIR,dst=/backup,readonly" \
      "$HELPER" sh -ec 'rm -rf /target/omnigrex-operations; tar -xzf /backup/microsandbox-journal.tar.gz -C /target'
  fi

  docker compose up -d --wait postgres
  docker compose exec -T postgres dropdb --username=omnigrex --if-exists omnigrex
  docker compose exec -T postgres createdb --username=omnigrex --owner=omnigrex omnigrex
  docker compose exec -T postgres \
    pg_restore --username=omnigrex --dbname=omnigrex \
    --exit-on-error --no-owner --no-acl \
    < "$BACKUP_DIR/omnigrex.dump"
)
```

An unavailable native inventory also aborts before state replacement; it is not empty-inventory evidence.
If native resources remain, preserve the current journal and resolve them through unchanged-configuration recovery before retrying restore.

Start the full stack and run preflight for every repository.
Restore the matching `.env`, secrets, compatibility artifacts, and exact Runtime Profile images on the new host first; retained Agent Sessions cannot continue when their pinned digests are missing:

```sh
docker compose up --wait
docker compose exec orchestrator \
  /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

Perform restore drills in a non-production environment.
Replace default volume names in both backup and restore commands when the installation uses overridden names; preserve the original logical namespace rather than remapping it.
A complete drill resumes a retained test Work Item, observes the same Participant and Agent Session/runtime identity and image binding, and confirms History Replay after fresh Execution Environments without a new model prompt.
History Replay currently has no operator-facing UI; use the source-checkout [Pi state recovery and upgrade gates](#pi-upgrade-and-state-recovery-qualification) for executable replay evidence.

Never use `docker compose down --volumes` during ordinary deployment, backup, or restore.
That command permanently removes all declared named volumes.

## Upgrades

Create a coordinated backup before every application, PostgreSQL, or Runtime Profile upgrade.

### Orchestrator-Only Upgrade

An orchestrator-only upgrade keeps the same configured agent digest/platform, protected historical images, and compatibility artifact.
Record the current commit and image references, download the target commit's common Pi bundle, selected backend overlay, and environment example, keep `OMNIGREX_PI_RPC_V1_IMAGE`, its platform, namespaces, and compatibility file unchanged, and update only the selected control-plane digest before validating Compose and deploying.
For the preparatory OpenCode stack use its legacy files and `OMNIGREX_OPENCODE_ACP_V1_*` binding instead.

```sh
docker compose config --quiet
docker compose up --wait
```

The orchestrator applies forward-only embedded PostgreSQL migrations under an advisory lock during startup.
There are no down migrations.
Do not downgrade an upgraded database in place.
Rollback after a migration requires restoring the coordinated pre-upgrade PostgreSQL and runtime-state backup together with the previous application revision and configuration.

For a PostgreSQL major-version upgrade, use a logical dump and restore into a new volume using the target PostgreSQL version.
Do not reuse a physical PostgreSQL data volume across major versions.

### Runtime Profile Image Change

Existing Agent Sessions remain pinned to the exact image that created them.
A source-to-candidate qualification does not migrate those sessions and does not prove candidate-to-source rollback compatibility.

Qualify the exact old-image to new-image upgrade in CI or another source checkout, not on the clone-free deployment host.
For Pi use the backend-scoped [Pi upgrade qualification procedure](#pi-upgrade-and-state-recovery-qualification); its results do not rebind protected Participants, and only new Participants select the current image.
The following OpenCode commands are transitional and do not qualify Pi images.
Packaging a host-side qualification tool is out of scope.
Pull both exact source and candidate image digests there, choose an absolute output path whose file does not yet exist, and run:

```sh
SOURCE='ghcr.io/jozala/omnigrex/opencode@sha256:SOURCE_DIGEST'
TARGET='ghcr.io/jozala/omnigrex/opencode@sha256:TARGET_DIGEST'
ARTIFACT="$PWD/runtime-profile-compatibility-results.json"
docker pull --platform=linux/amd64 "$SOURCE"
docker pull --platform=linux/amd64 "$TARGET"
OMNIGREX_PREVIOUS_OPENCODE_IMAGE="$SOURCE" \
OMNIGREX_PREVIOUS_OPENCODE_VERSION=1.18.19 \
OMNIGREX_OPENCODE_IMAGE="$TARGET" \
OMNIGREX_OPENCODE_VERSION=1.18.29 \
OMNIGREX_OPENCODE_ARCH=amd64 \
OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE="$ARTIFACT" \
mise run release-check-runtime-upgrade
```

Use the actual source and target versions.
The qualification creates the artifact with mode `0600`, and it refuses to overwrite an existing file.

Transfer the resulting JSON file to the deployment host (for example with `scp`), place it at an absolute host path outside the downloaded deployment directory, and set `OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE` to that path.
Set `OMNIGREX_OPENCODE_ACP_V1_IMAGE` to the candidate digest, pull that digest into the Docker Engine before startup, validate Compose, deploy, and run `doctor` for every repository.
Keep source and candidate digests available for every active and retained session.

An in-place Runtime Profile rollback is unsupported after Agent Participants using the candidate Runtime Profile exist unless the reverse direction has also been qualified.
The safe rollback is restoration of the coordinated pre-upgrade backup and previous deployment configuration.

## Manual Recovery

Omnigrex has no supported administrative command for editing Workflow records, requeueing internal jobs, or taking direct control of an Agent Session.
Do not edit PostgreSQL tables or runtime-state files manually.

Use this recovery sequence:

1. Read the `omnigrex:needs-human` comment and identify the failed prerequisite or uncertain mutation.
2. Correct GitHub App settings, repository access, credentials, images, storage, network access, or provider availability.
3. Recreate the orchestrator if configuration or image contents changed.
4. Run `doctor` until every check passes.
5. Confirm the Issue remains open.
6. Add `omnigrex:run` to create a new Workflow Attempt.

Re-adding the command label resets retry and review budgets but does not erase history.
Retained Agent Sessions are reused when their exact Runtime Profile image and state remain available.
If an earlier Developer Turn published a branch or opened a Pull Request but did not successfully request review, a later Turn for the same Agent Participant verifies that publication against its durable MCP mutations and fresh GitHub state before using it as in-progress work.
The later Turn starts at the verified published head and must explicitly request review; publication alone never schedules Reviewer.
An unproven branch, changed head, different Participant, or publication that cannot be matched to its recorded proposed commit (or, for older publications, its operation marker) causes a publication-conflict Human Handoff rather than automatic adoption.
If Git or GitHub cannot be reached during verification, execution follows bounded infrastructure retries instead of claiming a conflict.
Developer publication preserves committed Git history: an uncommitted workspace is rejected, while ignored local build artifacts are not published. A validated proposed commit is recorded before pushing; if an interrupted push cannot be corroborated on the branch, recovery does not reconstruct it from a later workspace and may require a Human Handoff.

For an apparently stuck deployment, inspect safe operational metadata:

```sh
docker compose ps
docker compose logs --since 15m orchestrator
docker ps --filter label=io.omnigrex.runtime-process=true \
  --format '{{.ID}} {{.Names}} {{.Status}}'
```

The orchestrator logs `MCP mutation failed` with a safe `failure_code`, operation and mutation identifiers, and allowlisted GitHub status/request ID or expected/observed commit IDs.
The corresponding `tool_invocations.last_error` contains the safe failure code and available allowlisted details for definite failures; unknown external outcomes remain subject to mutation reconciliation.
For read-tool failures, look for `MCP read failed` with the `agent_turn_id` and `tool_name`.
`failure_code` gives the broad classification, while `failure_stage`, `failure_reason`, and optional `validation_field` identify the processing step and specific failed check.
Detailed validation reasons initially cover issue lookup and review-thread reads; other read failures may have an `unclassified` reason.
For example, `github_invalid_response`, `review_threads_validation`, `missing_field`, and `review_thread.originalLine` identify a missing original line without disclosing a repository path or comment.
Cancellation and deadline expiry are classified before credential or GitHub errors are sanitized.

`github_http_status` is the actual status of the response associated with the failure, and `github_request_id` is its strictly validated GitHub request ID.
HTTP `200` does not imply GraphQL or tool success.
A missing status means response metadata is unavailable, not status `0` or proof that no request was sent.
A missing request ID can mean GitHub supplied none or its value failed validation.
Continuation failures use the response that exposed the failure, and final aggregate count mismatches use the last contributing response; earlier successful responses are never substituted for a request that received no response.
A scoped issue identity mismatch detected in the backend after a successful `GetIssue` has a precise `scoped_identity_validation` diagnostic but no response metadata because the successful-result interface does not carry it.

Authenticated shared gateway rejections emit `MCP request rejected`, including envelope, initialization, argument, authorization, and fence-validation failures.
Pre-dispatch failures can have no `tool_name`; arbitrary submitted tool names are never logged.
Use `agent_turn_id` to locate the Agent Turn, then filter by `diagnostic_call_id` to distinguish repeated calls and link a read failure to a subsequent recording failure.
Each rejected request emits one rejection entry; each failed read emits one execution entry and, if recording also fails, one additional entry at stage `read_recording` with the same ID.
Sending the final generic tool error does not add a duplicate rejection entry.
`duration_ms` measures elapsed processing from successful authentication to each log event, so the later recording entry can have a larger duration.
Keep diagnostic call IDs and GitHub request IDs as log fields, not Loki stream labels or metric labels.

`tool_invocations.last_error` and agent-facing read messages remain generic; these diagnostics never include arguments, GitHub response bodies, review text, or raw dependency errors.
In the transitional OpenCode path, `ACP MCP tool failed` is a runtime summary with a fixed `failure_class`, or `unclassified` for an unknown failure, rather than the authoritative backend cause.
Runtime tool-call IDs and gateway diagnostic IDs are separate identities with no guaranteed direct mapping.
Runtime-local failures that never reach the gateway and authentication failures without trusted live registration scope remain blind spots.
Stale-authorization logs cover checks after successful authentication, not all expired or removed registrations.
Mutation-specific planning, reservation, replay, and detached execution are outside this read-diagnostic correlation guarantee and retain their existing diagnostics.

An Agent Turn diagnostic containing `runtime_oom_killed` means Docker confirmed that its Runtime Process was OOM-killed before Omnigrex removed the container.
Omnigrex keeps the normal single infrastructure retry, then includes this diagnosis in the Human Handoff if the retry also fails this way.
Inspect the affected Agent Turn and the retry separately, including their Runtime Profile binding, last tool calls, and available Docker or orchestrator logs, before deciding whether to start another Workflow Attempt.
An EOF or exit code 137 without Docker's `OOMKilled` state does not establish an OOM cause; when the container was removed before inspection, the cause remains unknown.
Do not change the immutable Runtime Profile binding of an existing Assignment to adjust its memory limit.

Do not publish logs without checking them for repository content and operational identifiers.
The application redacts known credential types, but operator-added proxy and platform logs may not.

Restarting the orchestrator is safe when using the same configuration and images:

```sh
docker compose restart orchestrator
```

Startup reconciliation fences stale Runtime Processes and resumes durable work according to retry policy.
Use `docker compose up -d --force-recreate orchestrator` instead when the image, environment, or mounted configuration changed.

If the Developer webhook receives no deliveries, inspect the Developer App's Recent deliveries page, public DNS and TLS, proxy logs, firewall, and exact path.
If deliveries receive 401, verify that the local and GitHub webhook secrets match.
If preflight reports a missing Runtime Profile image, pull the exact digest without replacing it with a mutable tag.
If PostgreSQL or runtime-state restoration is required, stop automation and use the coordinated restore procedure rather than retrying against mismatched state.

## Rotation

For a GitHub App private key, create a new key in GitHub, replace only that App's local PEM file, recreate the orchestrator, run preflight, and delete the old key after success.
Never rotate both App identities to the same key.

Webhook-secret rotation is not atomic across GitHub and the deployment.
Stop the orchestrator, update the GitHub App and local secret during one maintenance window, recreate the orchestrator, and verify a real delivery before starting new work.

For provider credentials, replace the deployment bundle, recreate the orchestrator, run preflight, and use a controlled test Work Item to verify every configured model.
Preflight checks JSON shape but cannot prove live provider authorization without making a paid request.

After any secret rotation, securely remove superseded local and backup copies according to the operator's retention policy.

# OpenCode-to-Pi Migration

This is an archived procedure for [release commit 3125265](https://github.com/jozala/omnigrex/tree/3125265f30f5cbc97096babde01cbd8dae7388e5).
Its cutover scripts and tasks are retired from the current supported baseline; executable commands below apply to that recorded source revision.
Current installations follow [decision 0024](../decisions/0024-establish-a-clean-supported-release-baseline.md) and use fresh state.

This historical procedure replaces a Docker/OpenCode installation with a fresh Pi installation on Docker or Microsandbox.
It does not migrate conversations or a populated operational ledger.
For current Pi installation, operation, upgrades, and recovery, use the [operator guides](../operators/README.md).

## One-time OpenCode-to-Pi cutover

This is one explicit reset of OpenCode operational state and conversations, followed by a fresh Pi installation.
It implements [decision 0006](../decisions/0006-use-pi-through-an-agent-specific-adapter.md), not an in-place runtime upgrade.
The current source is Pi-only and contains no OpenCode deployment bundle or image build.
Obtain historical artifacts from the selected old release or its private recovery set.
OpenCode compatibility artifacts cannot qualify Pi Runtime Profiles.
The first operator-confirmed Pi/Microsandbox cutover and its [evidence boundaries](../plans/2026-10-13-pi-only-final-qualification.md#published-release-and-operator-cutover) are recorded separately from the full reset/recovery qualification procedure below.

### Preparation and shutdown

1. Select a release commit with successful Docker and KVM qualification, published exact Pi/control-plane images, public package pulls, and both exact-digest deployment smokes.
   Save the previous application revision, Compose files, `.env`, operator instructions, Profile revisions, and all exact protected image references in the private recovery metadata.
2. Rehearse on a separate host or with fully overridden installation resources using [the isolated gate](#isolated-cutover-rehearsal).
   Choose the Pi backend and provision its exact images and host prerequisites before touching the old installation.
3. Stop webhook ingress and pause human triggers for the maintenance window.
   Let admitted GitHub mutations settle; unresolved publication, stop, or corroboration evidence is a blocker to reset.
4. Using the old Compose file set, complete the [coordinated backup](../operators/backup.md) and verify its checksums and archive inventory.
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

Prepare the target deployment directory using [Pi installation](../operators/installation.md), export its exact Compose file set, and replace the provider secret with the Pi-native API-key bundle.
OpenCode's `type: api` credential entries must become literal Pi-native `type: api_key` entries; follow the current [secret-file procedure](../operators/secrets.md) for their placement and permissions.
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
export OMNIGREX_PI_CUTOVER_SOURCE_COMPOSE_FILE=/private/old-release/compose.yaml
export OMNIGREX_PI_CUTOVER_SOURCE_ORCHESTRATOR_IMAGE=REGISTRY/ORCHESTRATOR@sha256:SOURCE_DIGEST
export OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE=REGISTRY/AGENT@sha256:SOURCE_DIGEST
mise run test-pi-cutover
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-cutover
```

The equivalent host command without a mise installation is `OMNIGREX_PI_CUTOVER_REHEARSAL=1 OMNIGREX_PI_BACKEND=docker sh scripts/test-pi-deployment.sh`, or `OMNIGREX_PI_BACKEND=microsandbox` with its KVM group supplied.
A remote `DOCKER_HOST` alone is insufficient because Compose secrets are host-side files; stage the source on the daemon host and run there.
Select the trusted image-only old-release Docker Compose bundle from that release's commit-pinned download or private recovery set, with its exact matching published orchestrator and agent image pair.
The helper requires an available bundle file and full untagged registry SHA-256 image references before any Docker operation; it has no source-build fallback.
The supplied historical bundle must expose the original `postgres`, `opencode-image`, and `orchestrator` services and `backend`, `agent`, `postgres-data`, `runtime-state`, `workspaces`, and `mise-data` resource keys so the generated isolation override covers every installation resource.
The rendered source bundle must be image-only and contain only the covered service, volume, and network keys, fixture secret files, and standard Docker socket or disabled compatibility-file bind mounts.
Historical volumes must use the default or explicit `local` driver with no backing-store options; a generated volume name does not isolate bind-backed or network-backed storage.
An extra resource, external volume/network, custom volume driver, nonempty `driver_opts`, foreign bind mount, or source build context fails the gate before historical startup or cleanup authorization.
The gate isolates ambient configuration, creates unique networks/volumes/namespaces, starts that supplied historical stack, seeds synthetic disposal markers, validates a pre-reset dump/archive, removes the old resources, and starts the fresh Pi bundle.
It verifies readiness and independent doctor/MCP checks without paid model calls or real GitHub mutation authority.
It then checks coordinated Pi dump/state/capture recovery, numeric ownership, native-journal inclusion, and readiness after recreated containers.
Failure to back up or remove legacy storage stops the cutover before Pi startup.
All fixture resources and temporary secrets/backups are removed afterward; pulled source images remain cached.

For exact published-image rehearsal, additionally set both `OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE` and `OMNIGREX_PI_DEPLOY_IMAGE` to target registry digests.
The Microsandbox target uses its native control plane, while the old OpenCode source uses its Docker control plane.
Without the target pair the gate builds Pi target fixtures, which do not establish publication or no-login release evidence.
The old-release bundle and exact source pair are mandatory in every rehearsal.

The archive markers in this procedure test operational round-trip and disposal mechanics, not Pi conversation correctness.
Also run `test-pi-workflow` with `OMNIGREX_PI_WORKFLOW_TEST_PATTERN='^(TestPiControlledWorkflow|TestPiProtectedStateRecovery)$'` and `test-pi-upgrade`, or their Microsandbox equivalents, to qualify actual fenced Pi Sessions, protected ledger/state ownership, fresh-environment continuation, and replay without model activity.
External GitHub fixture failures are expected in the rehearsal; production requires all doctor checks, real webhook delivery, and the controlled Work Item above.
See [the cutover rehearsal evidence](../plans/2026-10-11-pi-operator-documentation-and-cutover.md) and [published-release status](../plans/2026-10-13-pi-only-final-qualification.md#published-release-and-operator-cutover) for the recorded results and qualification boundaries.

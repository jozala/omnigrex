# Qualify runtime behavior and release images

[Contributor guide](README.md) · [Testing](testing.md)

Run qualification in disposable environments with fixture authority, not against protected deployment data.
Use the selected backend's [host and storage prerequisites](../operators/backends.md).
For remote native gates, use an existing authorized `DOCKER_HOST` connection and the daemon host's KVM group.

## Production selection versus candidates

`agent/pi/selected-bundle.json` is the sole fresh-production bundle authority, not a generated checkout manifest.
`go run ./scripts/pi-bundle image`, `record`, or `qualification` reads and validates that selection for build scripts.
The record captures the exact owned-resource inventory, Node/mise checks, locked package-integrity resources, bootstrap contract, required executables, immutable golden digest and original source/run/attempt/smoke job IDs.
Production profiles, repository preflight and launches use that inventory; retained launches use the ledger inventory instead.
Initial fresh production selection is Linux/amd64 only, with an actionable error for arm64 and no checkout fallback.
Arm64 source-built candidate/protocol tests on Apple Silicon remain explicit and do not claim production qualification.
Explicit `candidate-amd64` / `candidate-arm64` CLI commands emit checkout inventories for isolated candidate tooling only.
Integration-only `NewCandidatePiV2` constructs source-built fixture contracts; it is absent from production binaries and is not a runtime configuration override.

Workflow/deployment qualification uses the selected golden image with the current orchestrator, not a rebuilt checkout substitute.
The controlled Workflow fixture adds only its test provider module to the selected image; its owned runtime files remain unchanged.
The Workflow/recovery suites cover fresh Developer/Reviewer preparation and subsequent Turns with unchanged pins under the current orchestrator.
The explicit candidate image/resource tests continue qualifying future runner edits separately.
To advance production, qualify the new exact bundle, review the complete record change, and follow the [coordinated cutover](../operators/upgrades.md#runtime-profile-image-change).
Never synthesize a qualified digest or publish an image just to populate a compatibility matrix.

## Pi protocol diagnostic

The disposable Pi 1.4.0 diagnostic exercises the adapter with a controlled HTTP provider, no real credentials, no GitHub/business MCP authority, no Workflow, and temporary tmpfs Session storage.
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

## Owned RPC event projection

The owned runner qualifies Pi 1.4.0's public runtime/Session subscription boundary, not a replacement RPC implementation.
Only the RPC subscriber receives projected events; the real Session, extensions, model context, and durable conversation keep their original messages.
`agent_end` is activity, never settlement authority; `agent_settled` and validated assistant stop/error evidence remain required.

| Outbound event | Control-channel projection |
| --- | --- |
| `agent_end` | Type and `willRetry`; no `messages` access or serialization |
| `message_start` | Type and role only; a rejected request can contain unsanitized error text before finalization |
| `message_end` | Type, message role, stop reason, sanitized error marker, provider generation, bounded provider diagnostic |
| `message_update` | Type, role, delta kind/index; stock Pi conversion retains tool-call start identity without the partial message; no text/thinking/argument deltas |
| `turn_end`, `entry_appended`, `queue_update`, `bash_execution_update` | Event type only; redundant message/tool/entry/queued-text/output bodies stay in the Session owner |
| `tool_execution_start/update/end` | Type, bounded diagnostic identity/parent identity, allowlisted name, boolean error flag, and optional valid native terminal `durationMs`; no arguments/results |
| `omnigrex_provider_request_failed` (owned request observer) | Closed failure class and revalidated diagnostic only; delivered to the RPC subscriber without adding Session messages, including native summary requests |
| `compaction_start`, `compaction_end` | Type and reason; end includes aborted/retry flags and a fixed failure marker, never a summary or raw provider error |
| `omnigrex_temporary_filesystems` (owned asynchronous sampler) | At most two fixed resource/statfs records, no filenames, file bodies, or error prose; separate optional coalescing slot, no budget/settlement evidence |
| Retry, settlement, other lifecycle and unknown events | Unchanged; required fields are not truncated |

The orchestrator still bounds every JSONL record to 1 MiB and fails closed on malformed/oversized required records.
Command responses are not filtered or re-correlated; production History Replay uses the separate bounded metadata operation, not native transcript-returning RPC queries.
Native transcript queries and unknown future events remain subject to the same orchestrator bound, not a promise of arbitrary transcript transfer.
Diagnostics use closed-vocabulary runtime observations, never transcript bodies or new raw-event logging.
An infrastructure protocol failure is not repaired by accepting a terminal intent without valid outcome evidence.

New Runtime Profiles include the selected bundle's projection module in their owned-resource hash inventory.
Historical inventories remain valid and retain their original behavior; release images still require both-backend qualification before deployment selection.
That release gate is distinct from optional qualification of an installation's historical source image against a candidate.
If long Turns fail on an old retained profile, inspect its selected runner/resources and qualify the replacement rather than increasing frame limits or changing settlement policy.
Run `node --test agent/pi/diagnostic/control-events.test.mjs agent/pi/diagnostic/temporary-filesystems.test.mjs` for the synthetic multi-megabyte projection boundary and the ordinary Go Pi tests for the real bounded-reader regression.
The configuration gate also runs `control-rpc.test.mjs` against the pinned SDK for long normal/retry/error/cancel completion, correlated commands during streaming, stdout backpressure, EOF cleanup, and fresh-process continuation.
The both-backend Session gate performs a multi-megabyte normal completion followed by fresh-environment continuation and bounded metadata replay.
Long-Turn fixtures accumulate several individually bounded assistant messages through read-tool calls, so only the redundant aggregate exceeds 1 MiB.
The separate durable Session limits remain unchanged: 1 MiB per entry and 256 MiB per file; projection does not make an oversized individual durable entry reopenable.
The SDK continuation test uses the owned durable Session opener as well, rather than bypassing its validation with the permissive Pi loader.
The controlled Workflow gate exercises long `request_review`, `submit_review`, and blocked completion through production launch, MCP, and reconciliation.

## Native compaction qualification

The configuration gate runs `agent/pi/diagnostic/compaction.test.mjs` on both backends against the pinned Pi 1.4.0 SDK and production runner settings, extensions, model metadata, Session opener, History Replay, and owned event projection.
Deterministic HTTP/SSE fixtures exercise native threshold summaries with queued same-assignment continuation and reduced context, explicit overflow compact-and-retry and its unsuccessful-recovery guard, summary failure with ordinary retries disabled, post-run and pre-prompt cancellation, and retained-context pre-prompt compaction.
Successful cases reopen the same identity, retain prior append-order history, and verify current-deployment instruction authority.
The same gate runs `compaction-compat.test.mjs` with controlled clocks to check strict timestamp separation, early timer wakeups, repeated summaries, and bounded continuation under frozen or rolled-back clocks.
Native qualification also cancels a queued continuation during the compatibility wait; the Go adapter tests distinguish requested, confirmed cancellation from unsolicited flags and completion winning an abort race.
The owned compatibility hook is a temporary Pi 1.4.0 workaround, not another compaction implementation; see [decision 0041](../decisions/0041-separate-pi-compaction-and-retry-timestamps.md).
The Go Pi tests cover actual event names, safe diagnostics, settlement only at `agent_settled`, cancellation across Pi's synthetic preflight error, and Turn-deadline-bounded prompt admission beyond the ordinary RPC timeout.
Failure-boundary fixtures check that explicit overflow survives sanitization but ambiguous quota/authentication/transport errors do not become overflow.
No live credentials or paid requests are used.
Dedicated combined compaction/step-budget qualification belongs to [#130](https://github.com/jozala/omnigrex/issues/130); these standalone fixtures do not claim it passed.
See [operator behavior and limitations](../operators/context-compaction.md).

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
The registration lives only in the isolated orchestrator-side volume and is logged out and remotely revoked on completion where possible.
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
Docker orchestrator wrappers use the daemon socket, while Pi environments never receive it.
Native wrappers use KVM and transferred image archives without a host Docker socket.
Fixture images remain cached.
See [Agent Turn evidence](../plans/2026-10-09-complete-pi-agent-turns.md) and [recovery evidence](../plans/2026-10-10-pi-failure-recovery.md).

## Pi upgrade and state recovery qualification

This optional gate qualifies source-to-candidate Session Continuation and History Replay on isolated copies, including separately restored archives.
A [normal supported Pi image rollout](../operators/upgrades.md#normal-rollout-without-session-migration) creates independent Sessions for new Participants and requires neither this historical source-to-candidate evidence nor artifact import.
Retained Participants keep their exact original image and contract; mandatory image/resource validation, historical-image availability, backup, backend provisioning, and repository doctor checks still apply.
Read [upgrade evidence](../plans/2026-10-10-pi-upgrades-and-state-recovery.md) and [backend-scoped qualification policy](../decisions/0017-bind-pi-qualification-to-backend-contracts.md).

```sh
mise run test-pi-upgrade
OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-pi-microsandbox-upgrade
```

Fixture gates use controlled provider/MCP servers, private volumes, and independently built source/candidate inventories.
The default upgrade gate uses the actual selected immutable baseline inventory against the explicit current-source candidate, with separate retained-R1 continuation and fresh-R2 Session probes.
These candidate probes are isolated evidence, not authorization to admit R2 in production before a reviewed selection change.
Set `OMNIGREX_PI_UPGRADE_FIXTURE_SOURCE=candidate` explicitly to exercise the older Pi 1.0.4 package fixture (including arm64 local builds); this does not represent a supported historical production release.
Synthetic aliases are test evidence and cannot emit deployment artifacts.

To produce compatibility evidence for actual source/candidate release images, provision both exact registry images and choose an absent private absolute output path:

```sh
OMNIGREX_PI_UPGRADE_SOURCE_IMAGE='REGISTRY/PI@sha256:SOURCE_DIGEST' \
OMNIGREX_PI_UPGRADE_TARGET_IMAGE='REGISTRY/PI@sha256:CANDIDATE_DIGEST' \
OMNIGREX_PI_UPGRADE_SOURCE_VERSION=1.0.4 \
OMNIGREX_PI_UPGRADE_TARGET_VERSION=1.4.0 \
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
Evidence is direction-sensitive and scoped to exact image/platform and Runtime Profile contracts, state format, and backend family/contract version; a forward pass does not establish reverse rollback compatibility.
Importing it records compatibility, not permission or machinery to migrate live Sessions, rebind Participants, convert state in place, or restore source state into a candidate-bound Participant.
No supported command performs those operations.
If import is configured, startup and doctor require evidence matching the exact configured target/backend contract; clear an obsolete optional import setting or replace it with separately qualified matching results, preserving historical artifacts and ledger evidence.

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
CI requires Docker and usable Linux/amd64 KVM, then repeats smoke after publication against current orchestrator and selected Pi digests.
Default deployment smoke builds only the current orchestrator and pulls the selected immutable Pi image.

## Qualify an unselected bundle before adoption

Ordinary main-push publication builds both orchestrators and the development image against the selected Pi bundle; it does not build, push, or capture a new Pi candidate.
To publish an unselected candidate, dispatch **CI** on `main` with `pi_mode: build-candidate` and leave `pi_candidate` empty.
After the required `check` succeeds, the main-only `build-pi-candidate` job publishes only Pi and retains `pi-candidate-build-<run>-<attempt>` (90 days) with the exact image-building source C, actual published candidate digest and complete inventory, without adopting it.
The package-write token is scoped to that job; no arbitrary source-ref input is accepted.
A successful build is not qualification, release readiness, or deployment guidance.
If publication or capture fails, do not substitute a tag or invented record; correct the failure and explicitly rerun the build.
Use that captured `pi-candidate.json`; do not regenerate C's hashes from a later checkout D.
For source-local investigation, the equivalent capture command in the image-building checkout C is:

```sh
OMNIGREX_PI_CANDIDATE_IMAGE=ghcr.io/jozala/omnigrex/pi@sha256:... \
OMNIGREX_PI_CANDIDATE_SOURCE_COMMIT=<full-C-commit> \
go run ./scripts/pi-bundle candidate-record > pi-candidate.json
```

This is a complete proposed inventory, not a production selection or a claim of prior qualification.
On the trusted default branch, explicitly dispatch the existing **CI** workflow with `pi_mode: qualify-candidate` and the `pi_candidate` input containing that JSON.
Missing or unknown modes, a candidate supplied to build mode, missing candidate JSON in qualification mode, and non-main dispatches fail validation before publication.
This qualification-only mode publishes no images or releases and changes no production selection or repository pins.
Both `qualify-pi-bundle-docker` and `qualify-pi-bundle-microsandbox` must succeed; they test the exact supplied digest/inventory through bounded resource checks and protocol using integration-only fixtures.
Mandatory bundle qualification (`OMNIGREX_PI_UPGRADE_MODE=bundle`) restores, replays and continues retained R1 Sessions only under their original R1 image/inventory, while R2 starts independent fresh Sessions with separate identities and storage.
It neither requires R2 to read R1 Session files nor emits Session-migration compatibility results.
The regular required Docker/Microsandbox gates use this independent mode too, so a failing optional cross-image probe cannot indirectly block adoption through the aggregate `check` or qualification run conclusion.
The existing `scripts/test-pi-upgrade.sh` defaults to the separate optional `compatibility` mode: use it explicitly for cross-image copied-state evidence when that capability is desired, not as a prerequisite for normal bundle adoption (see [decision 0012](../decisions/0012-qualify-repository-environments-without-rebinding-sessions.md)).
The qualification checkout D can differ from C; the captured inventory is used rather than D's checkout hashes.
For supported local Docker investigation, run `OMNIGREX_PI_CANDIDATE_FILE=/absolute/pi-candidate.json OMNIGREX_PI_BUNDLE_OUTPUT=/absolute/new-output OMNIGREX_PI_BACKEND=docker sh scripts/qualify-pi-bundle.sh`.

Only after successful qualification, propose a version-2 selection record retaining C as `source_commit`, with qualification `commit` D, `suite: pi-bundle/v1`, and the actual run, attempt and both job IDs.
Retain both `pi-bundle-<backend>-<run>-<attempt>` artifacts containing the tested `candidate.json`.
Release readiness verifies both successful job identities and both exact digest/platform/source/inventory receipts; successful jobs alone cannot authorize a different candidate.
Missing, expired, malformed or mismatched evidence fails closed: rerun qualification of the same exact image and review the replacement evidence, without rebuilding or changing its resources or repository pins.
The version-1 initial baseline is verified against its original deployment-payload artifact and successful product smokes, not against whichever Pi candidate that source commit happened to publish.
After selection, normal main-push product packaging and both exact-pair deployment smokes remain required before release designation or live adoption.

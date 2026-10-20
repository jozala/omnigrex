# Troubleshoot an installation

[Operator guide](README.md) · [Health and diagnostics](health-and-diagnostics.md)

Read the `omnigrex:needs-human` explanation and identify the failed prerequisite or uncertain mutation first.
Omnigrex has no supported administrative command for editing Workflow records, requeueing internal jobs, or taking direct Session control.
Do not manually edit PostgreSQL or retained Session files.

## Recover a blocked Workflow

1. Correct App permissions/access, credentials, exact images, storage, network, provider availability, or repository configuration.
2. Recreate the controller if image, environment, or mounted configuration changed.
3. Run repository doctor until every check passes.
4. Confirm that the Issue is open.
5. Ask the user to add `omnigrex:run` for a fresh Workflow Attempt.

Budgets reset without erasing history.
Retained Sessions are reused when their exact image and state remain available.
If coordinated data restoration is necessary, stop automation and use [restore](restore.md) rather than retry against mismatched state.

## Inspect a stuck deployment

Use the installation's selected Compose files:

```sh
docker compose ps
docker compose logs --since 15m orchestrator
docker ps --filter label=io.omnigrex.runtime-process=true \
  --format '{{.ID}} {{.Names}} {{.Status}}'
```

For Microsandbox also use the native inventory constraints in [backup](backup.md#stop-writers) and [backends](backends.md).
Only exact managed identities authorize adoption or cleanup; do not add labels to unrelated containers to make them look managed.

| Symptom | Next check |
| --- | --- |
| No Developer webhook deliveries | App event subscriptions, Recent deliveries, DNS/TLS, proxy logs, firewall, exact path |
| Delivery HTTP 401 | Exact match between local and GitHub webhook secrets |
| Missing labels | [Provisioning delivery and repository jobs](health-and-diagnostics.md#label-provisioning) |
| Readiness fails | PostgreSQL, backend access, protected images/state, compatibility results, healthcheck time budget |
| Missing Runtime Profile image | Provision the exact digest in the selected backend; do not substitute a mutable tag |
| Pi startup timeout | Turn/Session startup observations, working-revision tool installation, startup budget, and overall Turn timeout |
| Configuration Human Handoff | Repository owner's [configuration checks](../repository-owners/troubleshooting.md) and operator instruction validation |

INFO-level `Pi startup readiness started` and `Pi startup readiness completed` include Turn/Session identity, budget, duration, and safe outcome after RPC and durable Session preparation.
They do not identify a particular tool download or prove an uncertain execution stopped.
Doctor's successful shorter probe does not prove real working-revision bootstrap will complete.

## Retained Microsandbox native logs

For SDK failures, first inspect [SDK diagnostics](observability.md#microsandbox-sdk-failures) for the operation and original cause.
Before normal or recovery removal, Omnigrex attempts to copy complete available `runtime.log`, `kernel.log`, `exec.log`, and their positive-integer numeric rotations into:

```text
<OMNIGREX_RUNTIME_STATE_ROOT>/assignment-<participant-id>/runtime-logs/
  <agent-turn-id>-<execution-epoch>/<sha256-of-native-resource-id>/
```

The directory is beside the guest-mounted `runtime-state`, never mounted into the guest.
`archive.json` identifies the execution key, native resource, capture time, and copied/missing/failed file inventory.
Directories are private (`0700`); archived files are private (`0600`) under the orchestrator's UID/GID.
`runtime.log` contains host runtime diagnostics, `kernel.log` the guest kernel console, and `exec.log` raw primary-execution output, not every guest command or the full Session history.
All three are retained without severity filtering or redaction; treat them as sensitive private content.

With your installation's selected Compose files, locate and copy an archive without changing it:

```sh
docker compose exec -T orchestrator find /var/lib/omnigrex/runtime-state -type f -name archive.json
docker compose cp orchestrator:/var/lib/omnigrex/runtime-state/assignment-<participant-id>/runtime-logs/<agent-turn-id>-<epoch>/<resource-id-sha256> ./private-native-logs
```

Replace the angle-bracket placeholders and protect the local destination before sharing any content.
Archives survive environment removal and controller recreation through the existing runtime-state volume and share [Participant retention](retention.md#session-retention).
Only ordinary Agent Turn resources with valid identity receive Participant archives; diagnostic-only and malformed resources do not.

Copying starts after confirmed stop, before native removal, with a five-second per-resource budget limited by the cleanup deadline.
`Microsandbox native log archive incomplete` warns when preservation fails, but archival failure does not block safe cleanup.
Already completed file copies survive retries; partial files are never published as complete logs.
Missing source files are recorded rather than synthesized, and native rotation may already have discarded earlier entries.
There is no live follower: logs internally deleted by failed-create rollback or emitted during native removal may be unavailable.
An absent archive is not a readiness failure, and log contents do not establish stop, flush, or OOM receipts.

## Read MCP diagnostics

`MCP mutation failed` supplies a safe `failure_code`, operation/mutation identifiers, and allowlisted GitHub status/request ID or expected/observed commit IDs.
Definite failures place safe details in `tool_invocations.last_error`; unknown outcomes remain under reconciliation.

For reads, look for `MCP read failed` with `agent_turn_id` and `tool_name`.
`failure_stage`, `failure_reason`, and optional `validation_field` narrow the failure beyond `failure_code`.
Detailed validation initially covers issue lookup and review-thread reads; other failures can remain `unclassified`.
For example, `github_invalid_response`, `review_threads_validation`, `missing_field`, and `review_thread.originalLine` indicate a missing field without exposing content.
Cancellation/deadline classification precedes credential or GitHub sanitization.

`github_http_status` belongs to the response exposing the failure; HTTP 200 does not imply GraphQL or tool success.
Missing status means unavailable metadata, not status zero or proof of no request.
A missing request ID can mean it was absent or failed strict validation.
Continuation failures use the failing response, while aggregate mismatches use the last contributing response.
A backend scoped-issue identity mismatch can have precise validation diagnostics without response metadata because that interface does not carry it.

Authenticated gateway rejections emit `MCP request rejected` for envelope, initialization, argument, authorization, and fence failures.
Pre-dispatch failures can lack a tool name, and arbitrary submitted names are never logged.
Join repeated calls by Turn and `diagnostic_call_id`.
One rejection emits one entry; a failed read emits one execution entry and, if recording fails too, a `read_recording` entry sharing the ID.
The generic tool error adds no duplicate rejection entry.
`duration_ms` measures from authentication to each event, so the later entry can be larger.

Agent-facing read errors and `tool_invocations.last_error` remain generic for reads.
Diagnostics exclude arguments, response bodies, review text, and raw dependency errors.
Runtime-local calls and gateway diagnostic IDs have no guaranteed mapping.
Calls never reaching the gateway and authentication failures without trusted registration scope remain blind spots.
Stale-authority logs cover post-authentication checks, not every expired registration.
Mutation planning/reservation/replay/detached execution retain their separate diagnostics.

## Publication and terminal verification

A later Developer Turn can reuse prior published work only after matching durable mutations and fresh GitHub state for the same Participant.
It starts at the verified head and must explicitly request review; publication alone does not schedule the Reviewer.
An unproven branch, changed head, foreign Participant, or unmatchable commit/legacy marker causes a publication-conflict Human Handoff.
Unreachable Git/GitHub follows bounded infrastructure retry instead of claiming a conflict.
Publication rejects uncommitted work and excludes ignored build artifacts.
An interrupted push that cannot be corroborated is not reconstructed from a later workspace.

If a unique terminal intent succeeded before a lost response or eligible deadline, Omnigrex may accept the Turn after corroboration, retaining the prompt failure in diagnostics.
Explicit cancellation and ineligible terminal endings do not qualify.
Fresh-read failure can defer verification under the durable window without repeating the prompt or mutation.
Recovery proves runtime stop and reconciles mutations before pending verification.
See [user handoff behavior](../users/human-handoffs.md#when-publication-succeeded-but-verification-did-not).
For earlier crashes without a durable runtime ending, a successor receives prior-intent guidance and may use `confirm_prior_terminal_intent`; a review before known explicit cancellation cannot be confirmed or resubmitted in the retry.

## Out-of-memory evidence

`runtime_oom_killed` means Docker confirmed OOM before container removal.
The normal single infrastructure retry still applies; a repeated confirmed OOM appears in Human Handoff diagnostics.
Inspect the original Turn and retry separately, including bindings, tool calls, and available logs.
EOF or exit 137 alone does not establish OOM, and removal before inspection can leave the cause unknown.
Do not mutate an immutable Runtime Profile to change memory allocation; use the supported [memory setting](retention.md#agent-turn-memory).

## Restart safely

With unchanged configuration and images:

```sh
docker compose restart orchestrator
```

Startup fences stale processes and resumes durable work under retry policy.
Use `docker compose up -d --force-recreate orchestrator` when configuration or images changed.
Inspect logs for private content and identifiers before sharing them; application redaction does not cover every operator-added proxy/platform log.

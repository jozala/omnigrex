# Check health and repository readiness

[Operator guide](README.md) · [Troubleshooting](troubleshooting.md)

Use the selected deployment `COMPOSE_FILE` in every diagnostic shell.
Check health after installation, configuration changes, upgrade, restore, or credential rotation.

## Liveness and readiness

```sh
curl --fail http://127.0.0.1:8080/healthz
curl --fail http://127.0.0.1:8080/readyz
docker compose ps
```

Use the configured host port if different.
Startup verifies configured golden resources before Workflow workers begin.
Readiness continuously checks PostgreSQL, the selected backend, configured/protected exact images, retained state, and supported Runtime Profile history.
Docker writable-path probes use the selected immutable Runtime Profile.
Liveness may remain healthy during a dependency readiness failure.

The bundles set a 60-second readiness budget, a 65-second HTTP client deadline, and a 70-second Compose healthcheck deadline.
The client allows five seconds beyond `OMNIGREX_READINESS_TIMEOUT`.
If changing that setting, keep `OMNIGREX_HEALTHCHECK_TIMEOUT` at least ten seconds longer so Compose can receive the bounded result.

## Repository preflight

Run for each repository before its first trigger and after prerequisite changes:

```sh
docker compose exec orchestrator \
  /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

Require every result to be `PASS` and exit code 0.
Exit code 1 means a prerequisite failed; 2 means invalid command arguments.
Configuration errors are reported together before external checks, and independent checks continue after failures.

Checks cover configuration/secrets, PostgreSQL, backend resources, exact configured/protected images and state, GitHub Apps/installations, repository environments, Profiles, effective instructions/permissions, and authenticated runtime-to-MCP compatibility.
The optional compatibility-artifact check accepts an unset file; when configured, evidence must match the exact target/backend contract as described in [upgrades](upgrades.md#runtime-profile-image-change).
It does not require cross-image Session migration evidence for fresh Participants or waive protected-image/resource checks.
The PostgreSQL check is read-only and applies no migrations.
Resource inspection does not write named volumes.
GitHub checks create short-lived tokens but do not modify content or start a Workflow.

The Pi runtime check starts an isolated environment with a random exact Session identity, temporary `/state`, fixture-only provider auth, and authenticated diagnostic MCP authority.
It verifies model/thinking and discovery, submits no model prompt or business capability, and removes the environment before success.
It does not attach retained Participant state.
The MCP endpoint check expects the running orchestrator to reject unauthenticated requests with HTTP 401.
Doctor never prints keys, JWTs, installation tokens, webhook secrets, or provider credentials.

Passing doctor does not prove paid provider authorization or successful installation of working-revision tools in a real Turn.
In subscription mode, provider preflight also checks the running authentication manager's safe login, storage, and block status without refreshing tokens or submitting inference.
Use [authentication status and recovery](provider-authentication.md#recover-access) to correct those prerequisites.
Doctor has its own shorter outer diagnostic budget.
Verify actual repository build/tests in controlled work and real ingress through a GitHub delivery returning HTTP 202.

## Label provisioning

Developer App access asynchronously provisions five managed labels, creating missing names while preserving existing colors/descriptions and unrelated labels.
It applies no Workflow state and starts no work.
Installation-created deliveries enumerate all accessible repositories via paginated installation-token requests, bounded to 100 pages of 100 repositories.
Explicit additions and repositories created under all-repository access also queue provisioning.
Duplicate deliveries are harmless, and one repository failure does not block others.

When provisioning fails, inspect both `webhook_deliveries` and `jobs` through authorized read-only operational inspection.
`PROVISION_MANAGED_LABELS` jobs use `label-provisioning:<delivery-id>:<repository-id>` idempotency keys and repository payloads.
A failed delivery may have no jobs or may retain earlier committed batches; a processed delivery can have a later failed repository job.
Inspect status and `last_error` at both levels.
Provisioning jobs have no `normalized_event_id` because onboarding does not create normalized Workflow events.

Ingress rejects bodies over 25 MiB with HTTP 413 before authentication or recording; GitHub also omits deliveries beyond its own cap.
Transient conversion failures use increasing delays and GitHub rate-limit timing for up to eight attempts.
Large valid installations queue bounded batches with claim renewal.
Onboarding uses a separate processor and does not block Issue/Pull Request processing.

Startup does not backfill old installations, and idle deleted labels are restored only when a Workflow runs.
If onboarding was not delivered, predates provisioning, or exceeds the enumeration limit, create the missing `omnigrex:run` label manually.
The first Workflow then reconciles other missing managed labels.
Wait for trigger-label availability before asking users to start work.

## Source-checkout diagnostics

Protocol, live-provider, Workflow, recovery, and upgrade qualification use isolated source-checkout gates.
See [runtime qualification](../contributors/runtime-qualification.md) rather than running test fixtures against deployment state.
Those gates do not replace production repository preflight and real delivery verification.

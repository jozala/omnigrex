# ChatGPT subscription implementation

## Agreed contract

This plan implements [decision 0032](../decisions/0032-own-chatgpt-subscription-credentials-in-the-controller.md), agreed with Mariusz in the design interview.
One installation-wide ChatGPT account supplies subscription-backed `openai/model` requests.
An explicit deployment setting selects subscription or API-key authentication with no automatic billing fallback.

The controller owns durable login, verified identity, concurrent refresh coordination, idle renewal, and protected mutable secret storage.
The operator CLI supports headless browser login, status, logout, and explicit quota recovery.
Account replacement uses ordinary login without an active-Turn barrier, preserves the working login on failure, and affects subsequent requests from existing Sessions.
Runtime Processes retrieve access-only credentials before individual model requests through active execution-scoped authority.

Quota and reauthorization failures create separate durable admission blocks and Human Handoff.
Successful login clears an authentication block but not an independent quota block.
Explicit recovery permits another request without promising restored capacity or automatically resuming affected Work Items.
Temporary failures retain bounded retries.

## Delivery sequence

1. Implement the controller authentication manager, atomic credential storage, documented public-client OAuth flow, identity verification, refresh coordination, and lifecycle tests.
2. Add explicit configuration, controller-local operator commands, persistent deployment storage, and doctor diagnostics.
3. Connect the owned Pi runner to a private, fenced credential endpoint on the existing guest-reachable listener.
4. Preserve sanitized failure classifications through prompt evidence, recovery, and Workflow settlement without invalidating successful terminal mutations.
5. Update operator procedures and deterministic and opt-in live qualification on both backends.
6. Run focused tests, `mise run check`, Docker-backed integration, and available backend/live gates, recording actual results below.

## Verification requirements

Test concurrent renewal, login/refresh/logout ordering, persistence across recreation, long-Turn renewal, idle renewal, one-time OAuth state and PKCE, verified identity, and failed-login preservation.
Test active-Turn fencing and provider selection, independent persistent quota/auth blocks, explicit recovery, bounded temporary failures, and unchanged API-key operation.
Audit normal diagnostics and retained Session data for provider credentials.
Real subscription inference and tool use through the production runtime must be qualified on Docker and Microsandbox before describing the feature as release-qualified.

## Evidence

The controller authentication manager, operator CLI, scoped access-token endpoint, Pi request adapter, durable prerequisite settlement/recovery, deployment storage, doctor checks, and operator procedures are implemented.
Operational prerequisites use a runtime-neutral `PREREQUISITE_FAILED` outcome that enters Human Handoff without falsely marking an Agent Turn successful or consuming infrastructure retry.
Successful terminal mutations remain eligible for corroboration after a subscription failure, including interrupted cleanup.

Observed checks on 2026-10-20:

| Check | Result |
| --- | --- |
| `mise install` | Passed |
| `mise run check` | Passed after final runtime/parser changes |
| `mise run test-integration` | Passed, including subscription prompt evidence, early admission failure, interrupted-cleanup recovery, and terminal-intent preservation |
| `node --test agent/pi/diagnostic/subscription.test.mjs` | All 17 tests passed |
| `mise run test-pi-integration` | Passed after final runtime/parser changes on Docker |
| `mise run test-pi-microsandbox-integration` with remote Docker and the authorized host's KVM group | Passed after final runtime/parser changes on Linux/amd64 KVM |
| `OMNIGREX_PI_BACKEND=microsandbox sh scripts/test-pi-deployment.sh` on the daemon host | Passed image-only startup, disposable diagnostics, network isolation, persistence, readiness, authenticated webhooks, and coordinated ledger/state/capture/journal recovery |
| Offline subscription CLI in the rebuilt native controller image, with no network or GitHub configuration | Status and logout passed across separate containers using one disposable volume; directory/file ownership was 10001:10001 with modes 0700/0600 |
| `OMNIGREX_PI_BACKEND=docker mise run test-pi-subscription-live` | Passed, reported by Mariusz; `TestPiLiveSubscription/openai` used `openai/gpt-6.1-sol` and Pi fixture image `sha256:a0357a60e15308e143c1aafd09142766f7556021ca1495652de559fe8ca982de` |
| `OMNIGREX_PI_BACKEND=microsandbox mise run test-pi-subscription-live` with remote Docker and the authorized host's KVM group | Passed, reported by Mariusz; `TestPiLiveSubscription/openai` used `openai/gpt-6.1-sol` and Pi fixture image `sha256:d88593706cebaebf524871c3955e2b6ccb83e5c7ff1294d532d657c9d4d31c09` |

The full store/migration integration gate passed before the final JavaScript-only structured-error parser corrections.
Those corrections were subsequently covered by the final local gate and both backend Pi suites; the SQL and Go settlement implementation was unchanged.
Image-only deployment diagnostics deliberately use fixture App identities, so their expected external GitHub installation failures are not evidence of real repository access.

Docker image-only deployment remains blocked by the unchanged Alpine package lock: the package CDN returned HTTP 404 for `libexpat-2.8.5-r0` while building the controller image.
The selected Pi image, Docker runtime/store integration, and Docker protocol/configuration suites passed independently of that controller-image build.

Mariusz supplied successful terminal results for both real ChatGPT subscription gates on 2026-10-20.
Each run completed fresh browser authorization, production-runner preparation, real inference and an authenticated fixture-tool call, durable Session replay, and the stopped-state credential audit.
These operator-provided results complete the live subscription qualification requirement on both backends; they were not independently rerun by the implementation agent.
The separate Docker controller-image build blocker above still prevents claiming complete release-image qualification.
No production credential bundle or refresh-token snapshot was inspected or imported for these checks.

## Review

Two read-only subagent reviewers evaluated the complete change against its requirements and documented architecture in three rounds.
Important findings about diagnostic-prose classification, permanent model-access failure, oversized terminal events, and CR-only SSE framing were corrected and covered by regression tests.
Both reviewers reported no remaining important P1/P2 findings in the third round.

## Runtime endpoint namespace follow-up

The private credential endpoint moves from `/mcp/provider-auth` to `/runtime/provider-auth` so its URL identifies an internal runtime HTTP operation rather than an MCP protocol operation.
The Pi adapter resolves this path on the configured private listener's origin, retaining per-Turn bearer authorization and execution fencing.
Token retrieval and failure reports both use the new path; the old path is unavailable.
This requires matching rebuilt controller and Pi images, as with other owned-runner changes.
The live qualification results above describe the original subscription implementation before this endpoint change.

Follow-up verification passed: focused race-enabled MCP/Pi tests, all 17 subscription Node tests, `mise run check`, `mise run test-integration`, `mise run test-pi-integration`, and `mise run test-pi-microsandbox-integration` on the authorized Linux/amd64 KVM host.
Coverage verifies token retrieval and failure reporting on `/runtime/provider-auth`, retained authorization/fencing, and authenticated GET/POST rejection under the old MCP-prefixed path without invoking credential operations.
Two independent read-only subagent reviews reported no important P1/P2 findings for this namespace follow-up.

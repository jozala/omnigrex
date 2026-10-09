# Pi configuration, authenticated MCP, and instruction handling

## Prerequisite and implementation scope

This implements Step 10 of the [combined implementation plan](2026-10-05-pi-and-pluggable-execution-backends.md), following the [Step 09 protocol adapter](2026-10-09-pi-protocol-adapter.md).
It applies decisions [0002](../decisions/0002-use-current-deployment-agent-instructions.md), [0003](../decisions/0003-separate-personality-from-platform-and-workflow-guidance.md), [0006](../decisions/0006-use-pi-through-an-agent-specific-adapter.md), [0007](../decisions/0007-supply-provider-api-keys-through-disposable-files.md), and [0014](../decisions/0014-own-pi-resource-loading-with-a-pinned-runner.md), plus [ADR 0012](../adr/0012-load-repository-skills-from-the-current-checkout.md)'s checkout-local skill contract.

`internal/runtime/pi` renders trusted Profile inputs, translates local permissions into Pi's exact tool allowlist, bootstraps the owned runner over the execution transport, and verifies model/thinking selection before prompt admission.
`agent/pi/runner.mjs` constructs the pinned Pi SDK runtime and delegates JSONL protocol handling to Pi's existing RPC implementation.
`agent/pi/mcp-extension.mjs` authenticates against the exact gateway endpoint, preserves discovered input schemas and descriptions, and exposes authorized tools using the existing `omnigrex_` naming prefix.
`agent/pi/failure-extension.mjs` reconstructs failed assistant records through Pi's `message_end` extension hook before public listeners or Session persistence consume them.
`internal/runtime/agent.MCPEndpoint` replaces the gateway's ACP-specific descriptor; the temporary ACP adapter performs its own HTTP descriptor translation.
Immutable file-edit authority now belongs directly to Role Policy rather than its OpenCode-specific hardening settings.

The runner and extension are packaged in the existing pinned disposable diagnostic image and exercised by `omnigrex pi-diagnostic --configuration`, including a Reviewer option.
The image also packages checksum-locked ripgrep 15.1.0 and fd 10.3.0 for offline Pi `grep` and `find` execution on both supported architectures.
Operational commands remain authoritative in the [operator guide](../operator-guide.md#pi-protocol-diagnostic).

## Candidate Agent Profile schema

Profiles selecting `runtime: pi-rpc/v1` require an exact `provider/model` and accept optional `thinking` using `off`, `minimal`, `low`, `medium`, `high`, or `xhigh`.
The adapter checks actual model support rather than treating this vocabulary validation as qualification.
Pi Profiles reject OpenCode `steps` and `variant` fields, including zero or empty values.
The preparatory parser retains the current OpenCode Profile schema for the existing deployment path.

Pi local permissions are `read`, `edit`, `write`, `bash`, `grep`, `find`, and `ls`, with omitted permissions denied.
`skill: allow` authorizes discovery of `.agents/skills/` in the prepared checkout and requires `read: allow` to load its contents.
This special instruction permission does not register an additional Pi tool or expand MCP authority.
Reviewer `edit: allow` and `write: allow` are rejected independently of repository prose.
Unavailable OpenCode tools and delegation permissions are rejected in Pi Profiles.

## Runtime and authority behavior

The owned runner uses in-memory settings with untrusted project configuration and explicit resource-loader inputs.
It disables automatic extensions, native MCP, skills, prompt templates, themes, and context-file discovery, and supplies no project system-prompt source.
Current platform, Stage, Turn-purpose, startup-loaded operator, and repository Profile instructions are composed through the existing instruction owner.
Historical Session messages remain context and cannot become the next launch's system-prompt source.
Authorized skills are selected from physical checkout-local `SKILL.md` files; symlinks expanding discovery are rejected.

The owned extension requires authenticated discovery to match the complete expected capability set, including bounded pagination and duplicate rejection.
Tools retain gateway schemas without adding arguments, relaxing requirements, or substituting another endpoint.
The gateway continues validating Role authority, arguments, expiry, and the current Turn fence on every call.
Failed MCP operations retain a fixed safe error rather than headers or upstream source errors.
The transport's fetch policy checks the exact endpoint on every request and forbids redirects, including the SDK's separate SSE and reconnection paths.
An owned readiness marker is verified before the connection admits a prompt.

Preparation resolves an exact model from Pi's available models, applies that selection, validates requested thinking against Pi's reported supported levels, and reads back the effective model and thinking value.
Unsupported selections, missing MCP readiness, and silent thinking clamping leave the connection unprepared.
Startup failures use a fixed owned configuration-error record, preserving an explicit safe configuration failure instead of reporting an unexplained transport loss or retaining source diagnostics.
No fallback model or provider is requested.

## Credential placement

The Go renderer accepts Pi-native API-key entries, selects only the requested provider, and rejects empty keys, OAuth, and executable key references.
Environment references are rejected, while accepted literal dollar characters are escaped for Pi's actual file-backed auth reader and verified unchanged at the controlled provider.
Launch inputs travel in one bounded JSON frame over stdin instead of execution-backend environment metadata.
The runner writes selected credentials into `/run/pi-agent/auth.json` with mode `0600`; the diagnostic maps this directory to disposable tmpfs on both backends.
Per-Turn MCP headers remain in process memory and are not copied into Session files.
Configuration and endpoint debug formatting is redacted.
Provider failures and aborted responses retain their actual stop reason, the pinned Pi retryability distinction, selected model identity, and valid numeric usage counters.
Their error summaries are fixed safe strings; untrusted error bodies, partial content, response identifiers, and diagnostic metadata are discarded before Session append rather than scrubbed from files afterward.
The credential trust and operational recovery procedures remain in the [operator guide](../operator-guide.md).

## Acceptance tests and observed evidence

The shared integration suite uses the actual pinned Pi runtime, production owned runner and extension, production MCP gateway, and controlled HTTP provider through each backend's production transport.
Its in-memory gateway ledger and backend have no GitHub credentials or external side effects.

The suite verifies:

- Developer MCP read and Developer-only publication capability invocation, plus local write/edit behavior and actual `grep`, `find`, `ls`, and `bash` execution.
- Reviewer MCP read with unavailable Developer-only MCP capabilities and blocked Pi write/edit attempts.
- Rejection under the exact advertised MCP schemas and revoked Turn-fence rejection after successful discovery.
- Exact tool advertisement, all-local-tools-denied behavior, and poisoned project settings, extensions, MCP, system/append prompts, and automatic context exclusion.
- Authorized skill advertisement and content loading, disabled skills, and rejection of symlink-expanded skill discovery.
- Updated instructions with Pi-format history and structured system state retained by an actual prior Pi prompt in a separate process.
- Unsupported models and thinking values, invalid MCP tokens, and capability-set mismatch before admission.
- Selected-provider-only `auth.json`, private file permissions, and raw Session-file audits for provider keys and MCP tokens.
- HTTP provider errors echoing the supplied API key and MCP authority, plus failed partial responses containing those credentials, preserve failure classification without retaining credentials in actual Session files.
- Standalone protocol, Developer configuration, and Reviewer configuration diagnostic subprocesses on both backends.
- POST and optional SSE redirect rejection for five HTTP redirect statuses and both same-origin and different-origin targets, without delivering authentication to those targets.

The first integration attempt exposed a paused-stdin handoff between bootstrap and Pi's RPC reader.
The runner now resumes stdin after Pi attaches its JSONL reader; both complete backend suites exercise that corrected handoff.

Observed passing commands:

```sh
mise install
mise run check
mise exec -- go test -race ./internal/runtime/pi/... ./internal/agentprofile
mise run test-integration
mise run test-pi-integration
mise run test-pi-microsandbox-integration
mise exec -- go test -race -tags=integration ./internal/runtime/acp -run '^TestOpenCodeContinuedSessionUsesCurrentDeploymentInstructions$' -count=1
git diff --check
```

The Linux/amd64 KVM suite used the explicitly approved remote Docker host and its numeric KVM group.
Host addresses, credentials, and deployment metadata are intentionally excluded from this record.
The normal check and both complete Pi backend gates passed after the safe startup-error correction, including fourteen initial configuration scenarios and three standalone diagnostic variants.
The existing Docker integration gate passed, and the focused current-deployment instruction regression passed again after the shared platform guidance update.
Destructive stable-stack Compose recreation and live provider/GitHub gates were not part of this preparatory milestone.

### Independent review follow-up

The first independent Standards and Spec/correctness reviews reported three and two findings respectively, with the credential-template problem appearing on both axes.
The four distinct findings covered Pi's environment/dollar template interpretation, authenticated SSE redirects, missing offline search-tool binaries, and insufficient retained-system instruction evidence.
The renderer now rejects environment references and encodes accepted literal dollars, and the shared provider verifies the exact credential value after Pi reads its disposable auth file.
The extension applies an exact-endpoint fetch guard to every SDK transport path; twenty loopback regressions cover POST and optional SSE redirects across five statuses and two origin relationships.
The diagnostic image now contains checksum-locked search binaries and both Roles exercise actual `grep`, `find`, `ls`, and `bash` calls.
The retained instruction regression now creates its prior state through a real Pi prompt with old system instructions in a separate process before reopening that state under current instructions.
After these corrections, `mise run check` and both complete Pi backend gates passed with fifteen configuration scenarios and all three standalone diagnostic variants.
The second round used fresh independent Standards and Spec/correctness reviewers, each inspecting all twenty tracked changes and fourteen new files against the fixed pre-Step-10 baseline.
Both confirmed all four distinct findings resolved and reported zero new or unresolved actionable findings.
The Standards reviewer independently exercised the actual file-backed credential reader with eleven accepted literal cases and eight rejected environment-reference cases, ran all twenty redirect regressions, and reproduced instruction refresh across two fresh native processes.
The Spec/correctness reviewer independently reran all fifteen Docker configuration scenarios and three diagnostic command variants, and verified that the tested JavaScript files matched the current workspace.
Both reviewers passed the normal check and uncached focused race suites; Linux/KVM execution evidence comes from the complete developer-run shared gate.
After both clean second-round verdicts, the complete existing Docker integration gate passed again, and the final diff whitespace check passed.

### Provider failure retention follow-up

A subsequent human review reproduced an HTTP provider error copying its supplied API key into Pi Session history before Go's safe event normalization ran.
The shared failed-provider regression reproduced the same leak before the correction.
The correction installs a separate owned failure extension whose finalized-message replacement is applied before Pi notifies public listeners and calls `SessionManager.appendMessage`.
It reconstructs a narrow safe record, preserves failure/abort and retryability distinctions, and tolerates a context invalidated during shutdown so a caught extension exception cannot bypass sanitation.
The shared suite audits failed HTTP responses for both Roles and a failed streamed partial response, requiring actual retained files and an assistant error record with no provider or MCP credentials.
Focused extension-boundary regressions cover non-retryable and transient provider failures, quota errors, aborted content, secret-bearing diagnostic/usage fields, and invalidated contexts.

Building that regression also exposed a controlled-provider fixture defect: scenario dispatch compared only string user content, while Pi can encode that content as text-block arrays.
The fixture now normalizes both forms and rejects unknown scenarios, so the asserted capability and audit paths must actually execute.
The empty gateway invocation audit is also an explicit JSON array, allowing negative authorization checks to inspect zero executed calls.
Earlier fifteen-scenario acceptance claims are superseded by the corrected scenario decoding and expanded follow-up suite.
The corrected fixture also exposed missing executable discovery in native diagnostics, whose clean environment had no `PATH` and therefore could not find `/usr/local/bin/rg` or `fd` despite successful absolute-path execution.
The Pi diagnostic now supplies its executable path explicitly on both backends; the corrected capability checks exercise actual offline search calls through the selected transport.
After the provider-failure and fixture corrections, the normal check and both complete Pi integration gates passed, including nineteen configuration scenarios, six focused failure-hook regressions, twenty redirect regressions, and all three diagnostic command variants.
Fresh independent Standards and Spec/correctness reviews inspected all twenty tracked changes and sixteen new files and confirmed the human P1 and fixture/native-path defects resolved, with zero new actionable findings.
Both independently reran the corrected Docker scenarios and compared the seven owned JavaScript resources against the exercised image.
The Standards reviewer verified sanitation at the actual append boundary and used a no-op-hook negative control that reproduced credential retention, plus an unknown-scenario negative control that failed closed.
The Spec/correctness reviewer exercised real file-backed failure, retry success, streamed abort, retry-backoff cancellation, thrown-runtime fallback, and successful-content preservation, checking terminal listener events, Agent state, messages entering append, and Session-file bytes.
Remote Linux/KVM evidence for this follow-up remains the complete developer-run shared gate.
The final existing Docker integration command passed with cached results for unchanged packages, and the final diff whitespace check passed.

## Compatibility and next step

The existing OpenCode deployment remains operational through its original Profile schema and the ACP-owned endpoint translation.
That compatibility path is removed in Step 17.
The candidate Pi schema is preparatory and does not yet select production Agent Turns through the current Runtime Profile registry.
Step 11 adds durable Pi Session identity, continuation, and History Replay; Step 12 wires this configuration into full Agent Turns.
The retained-file instruction test qualifies resource/configuration freshness, not those later durable Store and state-ownership gates.
Published golden images, selected-backend operational rollout, and clean cutover remain assigned to later plan steps.

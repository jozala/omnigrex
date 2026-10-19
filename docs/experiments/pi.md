# Pi feasibility evidence

## Scope and pins

This record covers Step 02 of the [combined Pi and execution-backend plan](../plans/2026-10-05-pi-and-pluggable-execution-backends.md).
The original probe and reproduction commands are preserved in [the historical source](https://github.com/jozala/omnigrex/tree/415ba6d/experiments/pi).
The prototype image, RPC client, fixture extensions, and subscription snapshot have been retired in favor of production-adapter qualification.
Current optional API-key smoke runs through the production Pi runner; see [live-provider smoke](../contributors/runtime-qualification.md#live-provider-smoke).
It builds on the [Microsandbox topology proof](microsandbox.md).

The candidate is Pi 1.0.4 from `@earendil-works/pi-coding-agent`, Node 24.14.1, and Microsandbox 0.7.6.
Images are digest-pinned and npm dependencies are locked.
The old `@mariozechner/pi-coding-agent` package remains at 0.73.1; its older documentation is not the contract tested here.

## Observed behavior

| Boundary | Evidence |
| --- | --- |
| Process transport | Actual Pi RPC traverses Docker attach or Microsandbox streaming exec with separate stderr |
| Session identity | `--session-id` reopens the exact Participant-derived ID in a new container or microVM |
| Ambiguous creation | Discarding the initial state response and destroying execution still permits reopening that exact ID |
| Continued context | The controlled provider receives earlier conversation content after environment replacement |
| History Replay | `get_entries` returns persisted entries without adding a provider request |
| Prompt lifecycle | A prompt is acknowledged while a provider request is still held; completion requires `agent_settled` |
| Terminal outcomes | Ordinary completion, provider failure, and abort remain distinguishable through assistant outcomes |
| Deadline and process loss | A held request times out at the client; destroying execution after admission closes transport while prior history survives |
| Transient retry | One injected provider failure recovers after exactly one additional provider request |
| MCP | The owned extension authenticates, discovers a schema, executes the tool, rejects missing required arguments, and cannot execute after token revocation |
| Startup authentication | Invalid MCP authority makes extension loading fail and the Pi process exit |
| Resource exclusion | Poisoned project configuration and automatic context do not affect model input; only the explicitly configured skill is listed |
| Tool selection | The provider sees only `read` and the explicitly registered fixture MCP tool |
| Credentials | Raw retained files are scanned for the injected credential and MCP token; neither is present in the exercised paths |
| Live providers | API-key requests completed using `opencode-go/muse-spark-1.3-contributor` and `openai/o4-mini` on both backends |

The controlled provider and MCP service are real HTTP services outside the agent's container or microVM.
They do not replace the future acceptance test against Omnigrex's actual fenced MCP gateway.
The experiment supplies no GitHub credentials and makes no GitHub mutations.

Both primary live models passed on Docker and Microsandbox.
One final Docker rerun received an upstream `503 service_overloaded` from `opencode-go`; a single rerun of that check passed without changing the model.
The approved alternatives are `openai/gpt-6-luna` and `opencode-go/deepseek-v4-flash`, selectable explicitly through the documented environment overrides.
They were not needed for this milestone and have not been qualified by these runs.

## Verification commands

- `mise run check` passed.
- `mise run test-integration` passed with cached results for unchanged production packages.
- The controlled Pi suite and live API-key gates passed with both `OMNIGREX_PI_BACKEND=docker` and `OMNIGREX_PI_BACKEND=microsandbox`.
- The RPC concurrency regression passed under the race detector, including ten consecutive executions after its fix.
- Go vet for the integration-tagged experiment, JavaScript syntax checks, and shell syntax checks passed.

The required Step 02 feasibility evidence is sufficient to proceed to contract definition.
This is not a claim that a production Pi Runtime Profile or Microsandbox adapter has shipped.

## Findings that constrain the production adapter

### Verify thinking levels before changing configuration

Pi accepts `set_thinking_level=high` on the non-reasoning fixture model and clamps the effective value to `off`.
A successful command response is therefore insufficient evidence that the requested configuration was applied.
Query `get_available_thinking_levels`, reject unsupported selections before prompting, and verify the effective state after setting a supported value.
The experiment's strict-selection guard rejects the unsupported value and uses this checked path for live requests.

### Keep prompt acceptance separate from settlement

`prompt` reports admission, including a `disposition` such as `started`.
Observe final `agent_settled` and inspect the assistant outcome rather than treating admission or an intermediate run ending as successful completion.
Process or transport loss after admission remains uncertain execution, not ordinary completion.

### Keep the RPC reader independent of request waiters

Several commands can complete together or out of order.
The initial experiment client used a single-consumer notification channel, allowing one waiter to consume another waiter's notification.
A failing subprocess regression test reproduced this; broadcasting record availability fixed it.
The production adapter needs continuous reading, ID correlation, bounded transport shutdown, and explicit backpressure behavior rather than copying this small test client wholesale.

### Own resource loading explicitly

The tested startup combines `--no-approve`, resource-disabling flags, `--no-context-files`, explicit tool selection, and an explicit owned extension and repository skill path.
Project `.pi/settings.json`, extensions, MCP configuration, system/append prompts, and automatic context files cannot silently replace those choices in the tested fixture.
Pi 1.0.4 also includes native MCP support; this experiment deliberately disables it while proving the plan's owned-extension approach.
Whether native MCP simplifies the final design can be considered in the contract step without assuming it has been qualified here.

### Separate discovery from active authority

Successful MCP discovery does not confer permanent authority.
The test revokes the token after startup and verifies that the subsequent tool call cannot execute.
The production gateway must continue validating every call under its current Turn fence.
Pi may coerce some argument types before invoking a tool; gateway-side schema validation remains necessary.

### Qualify the complete network and image setup

The Node slim image did not include the system CA bundle needed by Microsandbox's image downloader.
The experiment copies the CA bundle from the pinned Microsandbox image.
Microsandbox tmpfs does not accept Docker-style `uid`/`gid` options; the controller establishes guest directory ownership before starting non-root Pi.
Live provider requests required the public-network profile; combining the no-network profile with a public-address rule alone did not pass the live checks.
The controlled suite instead uses the no-network profile with the exact fixture service IP allowed.

## Remaining qualification boundaries

- The experiment uses the Microsandbox CLI transport, not its Go SDK.
- It does not implement the production execution interfaces, durable Store binding, fencing, mutation reconciliation, or retained-state migration.
- Raw session-file audits cover these exercised calls, not arbitrary code intentionally reading and disclosing credentials supplied to its guest.
- The fixture extension and replay checks do not qualify the full Role Policy, human control, compaction history, or future Pi image upgrades.
- The optional subscription access-token snapshot test is unqualified and does not replace the successful OpenAI API-key gate.
- Production OAuth support remains outside the combined plan; refresh persistence and concurrency would need a separate decision.

The subsequent [Step 03 contract design](../plans/2026-10-07-runtime-boundary-contracts.md) uses these observations and records the consequential decisions before production refactoring.

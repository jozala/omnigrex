# Pi RPC feasibility experiment

This is Step 02 of the [combined runtime plan](../../docs/plans/2026-10-05-pi-and-pluggable-execution-backends.md).
It exercises a real pinned Pi process through Docker attach or Microsandbox's streaming exec transport.
The Go client, controlled provider, and MCP extension are experiment fixtures, not production adapters.
See the [evidence and integration findings](../../docs/experiments/pi.md).

## Pins

- `@earendil-works/pi-coding-agent` 1.0.4, including locked transitive dependencies.
- Node 24.14.1, using a digest-pinned `bookworm-slim` image.
- Microsandbox 0.7.6 from the digest qualified in Step 01.
- MCP SDK 1.32.1 and Zod 4.6.5.

The maintained Pi package has moved from the `@mariozechner` namespace to `@earendil-works`.
The experiment verifies the installed Node and Pi versions rather than trusting mutable image tags.
The Dockerfile uses the repository-root build context and copies only the canonical package manifests from `agent/pi/` and explicitly listed experiment fixtures.

## Controlled-provider checks

The controlled provider and authenticated Streamable HTTP MCP server run in a separate bounded Docker container.
An isolated Docker network connects them to the selected execution backend without publishing host ports.
The Microsandbox guest gets access to that exact fixture IP; controlled tests otherwise disable guest networking.
Each new Turn uses a new container or microVM and a fresh workspace and agent directory.
Only the explicit session-state mount survives execution replacement.

Run on a Docker daemon:

```sh
OMNIGREX_PI_BACKEND=docker mise run test-pi-feasibility
```

Run through an existing authenticated SSH connection to an approved Linux/amd64 KVM host:

```sh
export DOCKER_HOST=ssh://USER@LINUX_HOST
export OMNIGREX_MICROSANDBOX_KVM_GID="$(ssh USER@LINUX_HOST stat -c '%g' /dev/kvm)"
OMNIGREX_PI_BACKEND=microsandbox mise run test-pi-feasibility
```

Microsandbox's outer process is non-root and receives only the KVM device, not the host Docker socket.
The controller briefly uses guest-root exec to establish ownership of its disposable tmpfs directories, then starts Pi as UID/GID 10001.
The guest's Node base image and the copied Pi resources match the experiment's pinned versions.
This does not qualify a final golden image or the Microsandbox Go SDK.

The suite proves:

- Correlated commands and LF-only JSONL framing, including Unicode line/paragraph separators in response content.
- Prompt admission before completion, final `agent_settled`, provider errors, aborts, and a client deadline.
- Recovery from one transient provider failure with exactly two observed provider requests.
- Exact session identity across fresh environments and recovery after discarding a creation response.
- Previously persisted context surviving process destruction after prompt admission.
- History Replay using `get_entries` without a provider request.
- Authenticated MCP discovery and execution, required-argument validation, token revocation, and startup failure for an invalid token.
- Explicit tool and skill selection while poisoned project extensions, settings, MCP configuration, prompts, and context files remain excluded.
- Persisted-session checks for provider credentials and MCP tokens.
- Clear missing-model and missing-credential failures.

An independent subprocess regression test covers command waiters receiving several out-of-order replies together:

```sh
go test -race -tags=integration ./experiments/pi -run '^TestRPCClientCorrelatesConcurrentResponses$'
```

## Live API-key gate

Supply a local Pi-native `auth.json` with API-key entries for `opencode-go` and `openai`.
Use the [operator guide](../../docs/operator-guide.md) for the existing secret-handling rules.
Do not put the auth file in this experiment directory or record its contents in test evidence.

```sh
export OMNIGREX_PI_LIVE_AUTH_FILE=/secure/path/pi-auth.json
OMNIGREX_PI_BACKEND=docker mise run test-pi-live
OMNIGREX_PI_BACKEND=microsandbox mise run test-pi-live
```

The Microsandbox invocation also requires the Linux/KVM settings above.
Its live guest uses the public-network profile plus access to the fixture MCP endpoint.

The selected models are `opencode-go/muse-spark-1.3-contributor` and `openai/o4-mini`.
Each check requests a short `OK` response, selects a supported low thinking level, verifies the effective model/configuration, and audits persisted state.
Model selection never falls back to another provider or model.

To explicitly select the approved alternatives if a primary model becomes unavailable:

```sh
OMNIGREX_PI_OPENAI_MODEL=gpt-6-luna \
OMNIGREX_PI_OPENCODE_MODEL=deepseek-v4-flash \
mise run test-pi-live
```

Either override can be used independently.
The selected ID must match Pi's effective model exactly, and the result names the model actually tested.
These alternatives are not claimed as qualified until their corresponding checks pass.

The local test process reads the source auth file without changing it.
Only the selected provider's credential is sent through stdin into the execution environment's disposable agent tmpfs.
Real provider keys are absent from Docker arguments, environment configuration, image layers, and retained session mounts.
Diagnostic output is redacted using the selected key.
The secret file is never mounted into the fixture MCP server or the outer Microsandbox wrapper.

## Optional subscription snapshot experiment

`TestPiSubscriptionSnapshot` is separate from the API-key acceptance gate and remains unqualified.
It does not add production OAuth support.
It accepts an `openai` OAuth entry for Pi's current native subscription flow, not a legacy `openai-codex` entry.
It requires an access token with at least ten minutes remaining and withholds the real refresh token and client ID.
It therefore cannot rotate the operator's original credential.

```sh
OMNIGREX_PI_FEASIBILITY=1 \
OMNIGREX_PI_BACKEND=docker \
OMNIGREX_PI_OAUTH_AUTH_FILE=/secure/path/pi-oauth-auth.json \
go test -race -tags=integration -timeout 25m -count=1 -v ./experiments/pi -run '^TestPiSubscriptionSnapshot$'
```

Durable OAuth refresh, sharing refreshed credentials across fresh processes, and concurrent refresh coordination need their own design and qualification.

## Resource ownership and cleanup

The experiment owns random, uniquely named images, networks, state volumes, and containers.
Containers are labeled `io.omnigrex.experiment=pi-feasibility`.
Cleanup targets only those exact resources and reports failures, including connection failures during removal.
No Docker prune command is used.
The fixture server and Microsandbox wrapper have bounded lifetimes; normal cleanup also removes retained experiment volumes and temporary image tags.
If the client loses connectivity during cleanup, use the reported exact resource names for targeted cleanup after restoring access.

Docker probes limit Pi to one CPU, 1 GiB memory, and 128 PIDs.
The Microsandbox wrapper is limited to two CPUs, 3 GiB memory without additional swap, and 512 PIDs; each guest has one vCPU, 1 GiB memory, and a 15-minute maximum duration.
Run the two backend suites sequentially on a shared qualification host.

The regular `mise run check` remains required.
The experiment is opt-in and uses disposable resources independently of the deployed installation and regular Docker integration suite.

## Mid-prompt workspace capture

Step 05 adds `TestPiMidPromptWorkspaceCapture`, which holds an authenticated MCP response while a background writer mutates the workspace.
The controller suspends writers, transfers a stable representation, resumes them, and verifies that the same Pi prompt settles after the held response is released.
Run it with the backend and approved-host environment described above:

```sh
OMNIGREX_PI_FEASIBILITY=1 go test -race -tags=integration -timeout 25m -count=1 -v ./experiments/pi -run '^TestPiMidPromptWorkspaceCapture$'
```

The Microsandbox proof preserves versioned guest-stat xattrs because host modes and file-backed symlinks can differ from their guest-visible representation.
See the [capture evidence record](../../docs/plans/2026-10-08-location-independent-workspaces.md) for the observed result and production adapter obligations.

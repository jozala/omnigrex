# OpenCode 1.18.19 Compatibility Result

## Candidate

This bootstrap result covers OpenCode `1.18.19` on `linux/arm64` with ACP SDK `0.21.0` and protocol version `1`.
CI runs the same compatibility suite on `linux/amd64` as an additional smoke gate, but this recorded qualification result remains platform-specific to `linux/arm64`.
The arm64 musl release artifact is pinned by SHA-256 `cf5732a2665da6db7331fd4bff37a32eed71d684fef5e0956c0e29254600136b`.
The common agent image also pins mise `2026.8.10`, Alpine `3.24` by multi-platform digest, and every direct and transitive APK by version and SHA-256.
APK artifacts are installed with repository access disabled, so an `APKINDEX` update cannot change or remove the qualified dependency set.
Clean builds still require Alpine to retain the versioned files at its release CDN; retained registry-digest images, not reconstruction, are the artifact-recovery boundary.
Compatibility tests resolve the locally built tag to its immutable image ID before creating a Runtime Process.
A deployment Runtime Profile must use the pushed image's registry digest rather than the development tag or a local image ID.
OpenCode `1.18.19` was the latest release when implementation plan commit `0a72905` selected it on 2026-08-20 at 22:25 UTC; `1.18.20` was published on 2026-08-21 at 08:09 UTC.

## State Contract

Every Runtime Process uses `/workspace` as its working directory.
The entire `/home/opencode/.local/share/opencode` directory is persistent per Agent Assignment because it contains `opencode.db` and its possible `-wal` and `-shm` sidecars.
The workspace, OpenCode state, and assignment-local mise tools use separate persistent Docker volume subpaths.
The root filesystem is read-only, and all writable HOME, XDG, mise, and temporary paths are explicitly mounted.
The full writable-path contract is documented in [`agent/opencode/README.md`](../../agent/opencode/README.md).

## Automated Results

The following behavior passes against the real OpenCode binary through the Moby Engine API:

- ACP v1 initialization and required capability negotiation.
- Session creation at `/workspace`.
- Assignment-isolated discovery across a fresh Runtime Process.
- Ambiguous creation recovery through paginated `session/list` with exactly-one enforcement.
- When the persisted `session/new` response is discarded, a fresh Runtime Process recovers exactly one session through `session/list` without creating a duplicate.
- History Replay through `session/load` in a fresh Runtime Process.
- Session Continuation through `session/resume` in another fresh Runtime Process.
- A persisted conversation marker is included in the resumed model history and changes the response of a stateless fake provider.
- Workspace contents are deleted and recreated between Runtime Processes, and the continued Agent Session observes the replacement at the stable `/workspace` path.
- An ACP-provided Streamable HTTP MCP `2025-11-25` server is registered, advertises a tool to the model, and executes the requested side effect exactly once.
- After interruption between an MCP side effect and result delivery, a fresh Runtime Process can continue the same Agent Session and accept another prompt without repeating the side effect.
- After interruption during an MCP call before its side effect, a fresh Runtime Process can continue the same Agent Session and accept another prompt with no side effect recorded.
- After interruption during provider streaming, a fresh Runtime Process can continue the same Agent Session and accept another prompt.
- After interruption during a local shell tool call that has emitted output, a fresh Runtime Process can continue the same Agent Session and accept another prompt.
- Prompt cancellation aborts an active provider stream and returns terminal stop reason `cancelled`.
- ACP standard output is demultiplexed from Runtime Process standard error.
- The image and live process use UID and GID `10001` with all Linux capabilities dropped and `no-new-privileges` enabled.
- The Runtime Process uses a read-only root filesystem with bounded writable mounts.
- Provider configuration is supplied through the process environment, and the persisted runtime-state volume does not contain the test credential sentinel after normal operation.
- Under the Reviewer flags, project config, agents, MCP entries, skills, custom tools, permissions, configured instructions, and ACP capabilities match a clean Reviewer baseline; the configured MCP endpoint receives no request.
- Runtime-owned permission rules require ACP approval for bash and all other tools, and an unknown bash side effect is cancelled under both the clean and feature-branch-poisoned Reviewer workspaces.
- A feature-branch project plugin still executes during a Reviewer prompt despite `OPENCODE_PURE=true` and `OPENCODE_DISABLE_PROJECT_CONFIG=true`; the test records this exact compatibility boundary instead of treating pure mode as an execution sandbox.

Run the gate with:

```sh
mise run test-integration
```

## Controlled Upgrade

The previous-stable to candidate pair `1.18.18 -> 1.18.19` passes on `linux/arm64`.
The source arm64 musl artifact is pinned by SHA-256 `38a7504dab35007c70d2cdf89870b4be63faa60667318a90338798d34f53eab1`, and the target artifact uses the candidate hash recorded above.
The recorded local source image ID is `sha256:aee6e82c535bc6315f6b4d4954e6243815f27ae084c4e4a2faca5221c2f60bbf`, and the target image ID is `sha256:e4cc17155a89127422a152589f0f64fe5dfd6bf1b12c2fe3f4419fdb24d827d1`.

The source process creates a session, preserves model history, and executes the real Streamable HTTP MCP fixture under `1.18.18`.
After that process stops, the complete assignment subdirectory containing `opencode.db` and possible sidecars is copied into a separate candidate volume while the source volume remains mounted read-only during the copy and is never mounted into a candidate Runtime Process.
Fresh `1.18.19` processes replay history, continue the same session, produce a context-dependent response, and execute the MCP tool again without mutating the source state.
The complete raw ACP capability snapshots, including unknown fields, and newly created session configuration options are byte-for-byte equal between versions.
A trusted compatibility role is selected through `session/set_config_option` under both versions; both return identical selected-role options and include the role's distinct instruction in the resulting provider request.
The candidate's separate same-version tests cover create, dispose, continuation, History Replay, workspace replacement, and ambiguous or lost-response creation recovery.

The suite defaults to the local `1.18.18` and `1.18.19` tags but accepts already available digest-qualified references and expected versions through `OMNIGREX_PREVIOUS_OPENCODE_IMAGE`, `OMNIGREX_PREVIOUS_OPENCODE_VERSION`, `OMNIGREX_OPENCODE_IMAGE`, and `OMNIGREX_OPENCODE_VERSION`.

## Accepted Reviewer Limitations

OpenCode `1.18.19` executes an auto-discovered feature-branch project plugin during a prompt even when pure mode and project configuration suppression are enabled.
It can also discover nested `AGENTS.md` and `CONTEXT.md` files after a file read because the nested resolver does not honor `OPENCODE_DISABLE_PROJECT_CONFIG`.
A source comparison with OpenCode `1.18.25`, the latest release on 2026-09-01, found the same relevant plugin and nested-instruction paths and no additional suppression control.
The Control Owner accepted these exact limitations on 2026-09-01, so this result qualifies `1.18.19` without claiming that Reviewer Runtime Processes isolate arbitrary feature-branch code or instructions.
Provider credentials and network policy must therefore assume that feature-branch plugin code can execute inside the Reviewer Runtime Process.

A future candidate must be tested from a copied `1.18.19` state directory without mutating the original.

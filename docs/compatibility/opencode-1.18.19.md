# OpenCode 1.18.19 Compatibility Result

## Candidate

This bootstrap result covers OpenCode `1.18.19` on `linux/arm64` with ACP SDK `0.21.0` and protocol version `1`.
CI runs the same compatibility suite on `linux/amd64` as an additional smoke gate, but this recorded qualification result remains platform-specific to `linux/arm64`.
The arm64 musl release artifact is pinned by SHA-256 `cf5732a2665da6db7331fd4bff37a32eed71d684fef5e0956c0e29254600136b`.
The common agent image also pins mise `2026.8.10`, Alpine `3.24` by multi-platform digest, and every directly installed Alpine package version.
Compatibility tests resolve the locally built tag to its immutable image ID before creating a Runtime Process.
A deployment Runtime Profile must use the pushed image's registry digest rather than the development tag or a local image ID.

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
- History Replay through `session/load` in a fresh Runtime Process.
- Session Continuation through `session/resume` in another fresh Runtime Process.
- A persisted conversation marker is included in the resumed model history and changes the response of a stateless fake provider.
- An ACP-provided Streamable HTTP MCP `2025-11-25` server is registered, advertises a tool to the model, and executes the requested side effect exactly once.
- After interruption between an MCP side effect and result delivery, a fresh Runtime Process can continue the same Agent Session and accept another prompt without repeating the side effect.
- Prompt cancellation aborts an active provider stream and returns terminal stop reason `cancelled`.
- ACP standard output is demultiplexed from Runtime Process standard error.
- The image and live process use UID and GID `10001` with all Linux capabilities dropped and `no-new-privileges` enabled.
- The Runtime Process uses a read-only root filesystem with bounded writable mounts.
- Provider configuration is supplied through the process environment, and the persisted runtime-state volume does not contain the test credential sentinel after normal operation.

Run the gate with:

```sh
mise run test-integration
```

## Remaining Qualification

This is the first candidate, so no previous-stable to candidate upgrade result exists yet.
A future candidate must be tested from a copied `1.18.19` state directory without mutating the original.
The compatibility suite does not yet cover replacing workspace contents between Runtime Processes, actual loss of the `session/new` response after state persistence, interruption during provider streaming, a local tool call, or an MCP call before its side effect, or the complete Reviewer project-configuration isolation matrix.
OpenCode `1.18.19` can still discover nested `AGENTS.md` and `CONTEXT.md` files when reading files, so pure mode is not treated as a complete instruction sandbox.

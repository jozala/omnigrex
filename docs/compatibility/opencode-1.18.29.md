# OpenCode 1.18.29 Compatibility Result

## Candidate

This result covers OpenCode `1.18.29` on `linux/arm64` and `linux/amd64` with ACP SDK `0.21.0` and protocol version `1`.
The arm64 musl release artifact is pinned by SHA-256 `9b9044ede6449dc34888b85f0a51be5777a5886fe5e1cbd67898b13f8a5bec04`.
The amd64 musl release artifact is pinned by SHA-256 `294bfa0f4647e0fc5d3edceed94e8dc1047757975689f4269aadb91d6774f9ed`.
The common agent image also pins mise `2026.8.10`, Alpine `3.24` by multi-platform digest, and every direct and transitive APK by version and SHA-256.
A deployment Runtime Profile must use the pushed image's registry digest rather than a development tag or local image ID.

OpenCode `1.18.29` was selected because its bundled offline model catalog contains the required `opencode-go/muse-spark-1.3-contributor` model.
OpenCode `1.18.19` exposes only `opencode-go/muse-spark-1.2-contributor`, so it cannot enforce the requested Agent Profile through ACP session configuration.

## Automated Results

The complete Docker-backed integration suite passes against the native `linux/arm64` image.
The required-model assertion passes independently against both `linux/arm64` and `linux/amd64` images with runtime model fetching disabled.
The suite verifies same-version Agent Session creation, recovery, continuation, History Replay, cancellation, MCP operation handling, workspace replacement, provider-credential isolation, Reviewer controls, runtime readiness, and deployment topology.

Run the complete local gate with:

```sh
mise run test-integration
```

## Controlled Upgrade

The previous-stable to candidate pair `1.18.19 -> 1.18.29` passes on both `linux/arm64` and `linux/amd64`.
The source process creates a session, preserves model history, and executes the Streamable HTTP MCP fixture under `1.18.19`.
After that process stops, the complete assignment state directory is copied into a separate candidate volume without exposing the source volume to the candidate process.
Fresh `1.18.29` processes replay history, continue the same session, produce a context-dependent response, and execute the MCP tool again without mutating source state.
The complete ACP capability snapshots, newly created session configuration options, selected Role configuration, and durable state behavior remain compatible across the upgrade.

The recorded local arm64 image IDs are `sha256:0139b24c3001fee1dcd49931f4c46f801c5ffe91cc8bd79b0a6246e49c97167e` for `1.18.19` and `sha256:bf456fee0cb509c57c1a190d5920c235dd2911f4840a524350941c38abc69669` for `1.18.29`.
The recorded local amd64 image IDs are `sha256:ccb026cd3d88c0708aad0ecc83d171ccf975a5c29bda6342a285acf79a1232b9` for `1.18.19` and `sha256:9557269f67f25845d0695aa21f3bb35091b622d2a3be3a5e86ce0c126357e25a` for `1.18.29`.
These local image IDs are reproducibility evidence only and are not valid deployment Runtime Profile bindings.

## Accepted Reviewer Limitations

OpenCode `1.18.29` still executes an auto-discovered feature-branch project plugin during a prompt even when `OPENCODE_PURE=true` and `OPENCODE_DISABLE_PROJECT_CONFIG=true`.
It can also discover nested `AGENTS.md` and `CONTEXT.md` files after a file read because that resolver does not honor the project-configuration flag.
The Control Owner previously accepted these limitations for this Runtime Profile family, so this result preserves that boundary without claiming arbitrary-code or instruction isolation for Reviewer Runtime Processes.
Provider credentials and network policy must continue to assume that feature-branch plugin code can execute inside the Reviewer Runtime Process.

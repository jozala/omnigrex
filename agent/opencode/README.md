# OpenCode Runtime Process

This image packages OpenCode `1.18.29` and mise `2026.8.10` as checksum-verified release artifacts on a digest-pinned Alpine base.
Every installed APK, including transitive dependencies, is downloaded as an exact versioned artifact, SHA-256 verified, and installed without repository access.
Clean builds still depend on Alpine retaining those versioned artifacts at its release CDN, so pushed Runtime Profile images must be retained by registry digest rather than reconstructed as disaster recovery.
The process runs as UID and GID `10001` and starts `opencode acp` in `/workspace`.

## Runtime Paths

Every Runtime Process must mount the assignment workspace at exactly `/workspace`.
The Engine adapter binds configured source volume names to exact targets and requires a non-empty assignment subpath for every volume mount.
OpenCode stores its SQLite database at `/home/opencode/.local/share/opencode/opencode.db` because `XDG_DATA_HOME` is `/home/opencode/.local/share`.
SQLite may create `opencode.db-wal` and `opencode.db-shm` beside the database, so the entire `/home/opencode/.local/share/opencode` directory is the atomic persistent runtime-state boundary.
Do not copy only the main database while a Runtime Process is active.

The writable paths are:

| Purpose | Path | Lifetime |
| --- | --- | --- |
| Assignment workspace | `/workspace` | Disposable between Agent Turns |
| OpenCode database and sidecars | `/home/opencode/.local/share/opencode` | Persistent per Agent Assignment |
| Repository tools installed by mise | `/home/opencode/.local/share/mise` | Persistent per Agent Assignment |
| Runtime-owned OpenCode configuration | `/home/opencode/.config` | Disposable or read-only injected configuration |
| OpenCode cache | `/home/opencode/.cache` | Disposable |
| OpenCode state | `/home/opencode/.local/state` | Disposable |
| OpenCode home configuration scratch space | `/home/opencode/.opencode` | Disposable |
| Temporary files | `/tmp/opencode` | Disposable |

Provider credentials must be supplied through process environment variables and must not be written to the persistent OpenCode data directory.
`OPENCODE_AUTH_CONTENT={}` prevents credentials from being sourced from a persisted auth file.
Automatic updates and session sharing are disabled through both environment guards and runtime-owned inline configuration.

## Reviewer Isolation

Reviewer Runtime Processes must additionally set the following environment variables:

```text
OPENCODE_PURE=true
OPENCODE_DISABLE_DEFAULT_PLUGINS=true
OPENCODE_DISABLE_PROJECT_CONFIG=true
OPENCODE_DISABLE_EXTERNAL_SKILLS=true
OPENCODE_DISABLE_CLAUDE_CODE=true
```

Pure mode alone does not disable project configuration, instructions, skills, or MCP servers.
Reviewer configuration must also omit static MCP entries and pass only orchestrator-approved `mcpServers` in ACP session requests.
Runtime-owned configuration must require ACP approval for every tool category and explicitly override branch-configurable tools such as bash; the ACP decision function cancels every permission request not authorized by the Agent Profile.
The compatibility suite verifies that project config, agents, MCP entries, skills, and configured instructions do not reach the Reviewer model under these flags.

OpenCode `1.18.29` still executes an auto-discovered project plugin during a prompt even when `OPENCODE_PURE` and `OPENCODE_DISABLE_PROJECT_CONFIG` are both `true`.
It can also discover nested `AGENTS.md` and `CONTEXT.md` files when reading files because that resolver does not honor the project-config flag.
OpenCode `1.18.29` retains the same relevant plugin and nested-instruction paths and does not expose another suppression control.
The Control Owner accepted these limitations for this compatibility profile on 2026-09-01, but the Reviewer Runtime Process is not an arbitrary-code or instruction-isolation boundary and its provider credentials and network access must be treated accordingly.

## Local Validation

Build the image with:

```sh
mise run agent-image
```

Build both the current and previous images required by the controlled-upgrade suite with:

```sh
mise run agent-images
```

The integration suite accepts already available digest-qualified image references, expected versions, and an explicit target architecture through `OMNIGREX_PREVIOUS_OPENCODE_IMAGE`, `OMNIGREX_PREVIOUS_OPENCODE_VERSION`, `OMNIGREX_OPENCODE_IMAGE`, `OMNIGREX_OPENCODE_VERSION`, and `OMNIGREX_OPENCODE_ARCH`.
For example:

```sh
OMNIGREX_PREVIOUS_OPENCODE_IMAGE='registry.example/opencode@sha256:source' \
OMNIGREX_PREVIOUS_OPENCODE_VERSION='1.18.19' \
OMNIGREX_OPENCODE_IMAGE='registry.example/opencode@sha256:candidate' \
OMNIGREX_OPENCODE_VERSION='1.18.29' \
OMNIGREX_OPENCODE_ARCH='amd64' \
go test -race -tags=integration ./internal/runtime/acp
```

Validate the bundled tools and non-root identity with:

```sh
docker run --rm --entrypoint sh omnigrex/opencode:1.18.29 -c \
  'test "$(id -u)" = 10001 && opencode --version && mise --version && git --version'
```

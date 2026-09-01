# OpenCode Runtime Process

This image packages OpenCode `1.18.19` and mise `2026.8.10` as checksum-verified release artifacts on a digest-pinned Alpine base.
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
OPENCODE_PURE=1
OPENCODE_DISABLE_DEFAULT_PLUGINS=1
OPENCODE_DISABLE_PROJECT_CONFIG=1
OPENCODE_DISABLE_EXTERNAL_SKILLS=1
OPENCODE_DISABLE_CLAUDE_CODE=1
```

Pure mode alone does not disable project configuration, instructions, skills, or MCP servers.
Reviewer configuration must also omit static MCP entries and pass only orchestrator-approved `mcpServers` in ACP session requests.
OpenCode `1.18.19` can still discover nested `AGENTS.md` and `CONTEXT.md` files when reading files, so runtime permissions and trusted instructions must not depend on pure mode as a complete instruction sandbox.

## Local Validation

Build the image with:

```sh
mise run agent-image
```

Validate the bundled tools and non-root identity with:

```sh
docker run --rm --entrypoint sh omnigrex/opencode:1.18.19 -c \
  'test "$(id -u)" = 10001 && opencode --version && mise --version && git --version'
```

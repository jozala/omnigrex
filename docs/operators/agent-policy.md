# Configure deployment-owned agent guidance

[Operator guide](README.md) · [Documentation hub](../README.md)

Omnigrex supplies platform instructions, Stage objectives, completion criteria, and purpose-specific guidance.
Repository Agent Profiles supply preferences, personality, communication style, and skill selection.
The operator can add common and Role-specific guidance without changing repository files.

Precedence is platform/Stage contracts, operator Role guidance, operator common guidance, then repository preferences.
Capabilities and transitions remain code-enforced rather than granted by instruction text.
The running deployment's instructions supersede older deployment guidance in retained Session history.

## Mount the instruction directory

Optionally configure `OMNIGREX_AGENT_INSTRUCTIONS_DIR` as a clean absolute path inside the orchestrator:

```text
agent-instructions/
  common.md
  roles/
    DEVELOPER.md
    REVIEWER.md
```

Use exact Role IDs, not repository Profile names.
The common file and individual Role files are optional; an unset directory adds no operator guidance.
A configured missing/unreadable directory, unexpected entry, unknown Role, non-text file, invalid UTF-8, NUL byte, or file over 64 KiB fails startup and preflight.
Common plus applicable Role text must fit a combined 64 KiB JSON-encoded budget, including escapes.
Empty files add no guidance.

Create `compose.instructions.yaml` alongside the deployment files:

```yaml
services:
  orchestrator:
    environment:
      OMNIGREX_AGENT_INSTRUCTIONS_DIR: /etc/omnigrex/instructions
    volumes:
      - type: bind
        source: ./agent-instructions
        target: /etc/omnigrex/instructions
        read_only: true
        bind:
          create_host_path: false
```

Create the host directory, make files readable by the controller, and append `:compose.instructions.yaml` to the already selected `COMPOSE_FILE` once.
Preserve the common bundle, backend, and storage overrides in the same file set.
The bundles forward the variable, but you must supply the corresponding mount.
Only the orchestrator receives the directory; Runtime Processes receive composed text over stdin, not host paths or an environment entry.

## Apply guidance changes

Files are loaded once at orchestrator startup.
After editing them, recreate the controller with the same Compose selection:

```sh
docker compose up -d --force-recreate orchestrator
```

New launches, including continuation and recovery, use current binary/Stage guidance and startup-loaded operator text.
There is no persisted composed-instruction snapshot to replay and no hot reload inside an executing Turn.
Repository Profile provenance follows its separate default-branch rules.
See [decision 0002](../decisions/0002-use-current-deployment-agent-instructions.md) and [decision 0003](../decisions/0003-separate-personality-from-platform-and-workflow-guidance.md).

## Approve tool-path variables

Set `OMNIGREX_AGENT_PATH_ENV_ALLOWLIST` to comma-separated approved names such as `TMPDIR,GOTMPDIR,GOCACHE,GOPATH`.
The allowlist is shared across Roles.
Repository owners can request disk-backed directories and lifecycles through [tool-path configuration](../repository-owners/tool-paths-and-caches.md), not arbitrary host path values.
Runtime-control and credential names such as `PI_*`, `OMNIGREX_*`, `NODE_OPTIONS`, `HOME`, and `PATH` remain forbidden even if listed.
Review storage capacity and [cache monitoring](retention.md#tool-cache-monitoring) when approving persistent caches.

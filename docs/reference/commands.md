# Command reference

[Reference](README.md) · [Operator guide](../operators/README.md)

The image entry point runs the orchestrator without a subcommand.
Use `/usr/local/bin/omnigrex` inside the selected deployment service for operational commands.
Preserve the exact `COMPOSE_FILE` and mounts.
Unknown commands and invalid arguments return exit code 2.

## doctor

```sh
docker compose exec orchestrator /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

`--repository` is required; positional arguments are rejected.
Exit codes are 0 for all-pass, 1 for prerequisite failure, and 2 for invalid arguments.
Independent checks report `PASS`/`FAIL` lines.
See [preflight effects and limits](../operators/health-and-diagnostics.md#repository-preflight).

## healthcheck

The Compose healthcheck invokes `omnigrex healthcheck` using `OMNIGREX_HEALTHCHECK_URL`.
The default is `http://127.0.0.1:8080/readyz`.
The HTTP client budget is readiness timeout plus five seconds; align the outer Compose timeout using [health settings](../operators/health-and-diagnostics.md#liveness-and-readiness).
It returns 0 on success and nonzero on configuration, request, or response failure.

## state export and state restore

Both require `--participant` with the exact Agent Participant UUID.
`state export` streams a checksummed archive to stdout; `state restore` reads it from stdin.
They operate offline under writer/collection barriers and use a ten-minute deadline.
Never run them against active retained state.

Use the complete [export procedure](../operators/backup.md#supplemental-session-exports) and [restore procedure](../operators/restore.md#restore-a-supplemental-session-export), including staging and matching ledger requirements.
Participant-only archives are supplemental and omit PostgreSQL/protected captures.

## pi-diagnostic

The [source-checkout procedure](../contributors/runtime-qualification.md#pi-protocol-diagnostic) builds and provisions the disposable image before running this command.

| Flag | Contract |
| --- | --- |
| `--image` | Required exact local image ID or registry digest, already available |
| `--backend` | `docker` (default) or `microsandbox` |
| `--arch` | `amd64` (default) or `arm64` Linux guest |
| `--configuration` | Exercise owned configuration and authenticated fixture MCP |
| `--reviewer` | Verify Reviewer restrictions; requires `--configuration` |
| `--sessions` | Exercise disposable Session coordination |
| `--namespace` | Diagnostic ownership namespace; backend-specific default |
| `--microsandbox-home` | Private native cache/inventory, default `/tmp/msb-pi` |
| `--microsandbox-executable` | Default `/usr/local/bin/msb` |
| `--microsandbox-firmware` | Default `/usr/local/lib/libkrunfw.so.5.6.1` |

It accepts fixture authority rather than real provider credentials and creates no Workflow.
Use separate diagnostic storage, not protected Participant state.

## Development tasks

Mise tasks are development entry points, not installed administrative commands.
See [testing](../contributors/testing.md) and [mise.toml](../../mise.toml) for exact task definitions and required host capabilities.

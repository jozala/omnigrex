# Configure tool paths and caches

[Repository-owner guide](README.md) · [Documentation hub](../README.md)

Use `.omnigrex/turn-configuration.yaml` when repository tools need disk-backed temporary directories or caches retained across Agent Turns.
Without this file, `TMPDIR` uses the Runtime Profile's bounded `/tmp` path.

## Agree on approved variables

Ask the operator to approve variable names through `OMNIGREX_AGENT_PATH_ENV_ALLOWLIST`.
For example, Go builds may need `TMPDIR,GOTMPDIR,GOCACHE,GOPATH`.
The allowlist is shared across Roles, and the repository requests names and lifecycles rather than supplying host paths.
Runtime-control and credential-related names, including `PI_*`, `OMNIGREX_*`, `NODE_OPTIONS`, `HOME`, and `PATH`, are always rejected.

## Publish the configuration

```yaml
version: 1
environment-paths:
  directories:
    build-tmp:
      lifecycle: turn
    go-build-cache:
      lifecycle: assignment
  environment:
    - name: TMPDIR
      directory: build-tmp
    - name: GOTMPDIR
      directory: build-tmp
    - name: GOCACHE
      directory: go-build-cache
```

Publish the file on the default branch.
It is read from the same pinned commit as Agent Profiles and applies to both Roles.
Unknown fields, unsupported sizes, unapproved variables, and invalid directories produce a configuration Human Handoff before Runtime Process launch.
See the [configuration reference](../reference/repository-configuration.md#tool-paths) for the validation source.

## Understand the lifecycle

Requested directories are isolated on disk-backed tool storage outside the workspace and Change Proposal tree.
Turn scratch is removed after the Runtime Process stops.
The `assignment` lifecycle keeps Participant-local caches across Turns and removes them during Assignment collection.
Approved directory bindings take precedence over conflicting mise activation.

These paths have no per-Participant disk quota.
The operator should monitor storage and [cache warnings](../operators/retention.md#tool-cache-monitoring).
Moving build output to disk does not increase execution memory or limit compiler parallelism.

Next: [Repository troubleshooting](troubleshooting.md).

---
status: accepted
---

# Control Docker directly from the orchestrator

The first deployment mounts the Docker Engine socket into the non-root orchestrator so it can create isolated Runtime Processes without a separate runner service or nested Docker daemon.
This is the smallest reliable Docker Compose topology, but access to the socket grants the orchestrator effective host-root authority regardless of its Unix user.

## Consequences

The orchestrator is part of the trusted computing base and must expose only deterministic, validated container operations.
Agent containers never receive the Docker socket, run as non-root users, and use restricted capabilities, resource limits, isolated workspace mounts, and read-only root filesystems.
Potentially hostile repositories are outside the first-iteration threat model.

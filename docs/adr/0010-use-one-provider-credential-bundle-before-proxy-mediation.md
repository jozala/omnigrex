---
status: accepted
---

# Use one provider credential bundle before proxy mediation

The current OpenCode runtime accepts one authentication object through `OPENCODE_AUTH_CONTENT`.
Omnigrex therefore reads one deployment-wide bundle from `OMNIGREX_PROVIDER_CREDENTIALS_FILE` and injects it ephemerally into each Runtime Process.

## Consequences

The credential bundle is never persisted in Workflow, Participant, Session, Turn, job, event, or runtime-state records.
Every Runtime Process can technically access every provider credential in the bundle, so Agent Profile Role separation is not a provider-secret isolation boundary.
Operators must restrict provider privileges, configure spending controls, and rotate the entire bundle after suspected runtime compromise.
Future stronger isolation should mediate provider requests through an orchestrator-controlled proxy rather than distributing provider secrets to Runtime Processes.

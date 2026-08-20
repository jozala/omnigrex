---
status: accepted
---

# Use ACP as the agent runtime boundary

The orchestrator communicates with coding-agent runtimes through Agent Client Protocol version 1 instead of runtime-specific CLI output or APIs.
ACP is adopted in the first iteration because persistent sessions, streamed interaction, cancellation, future human participation, and runtime substitution are foundational requirements rather than optional integrations.

## Consequences

Session Continuation and History Replay remain separate Omnigrex capabilities even when one ACP method supplies both.
Runtime Profiles are immutable and versioned, and their stable working directory is part of session compatibility.
ACP session identifiers remain opaque and runtime-specific, so replacing a runtime implementation creates a new Agent Session rather than migrating the old one.
Compatibility tests between exact Runtime Profile versions inform future migrations, but the first iteration keeps existing sessions pinned.
The first implementation provides a narrow Go ACP client over standard input and output and validates it against OpenCode.

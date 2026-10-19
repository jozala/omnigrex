# Plans and specifications

These documents record accepted intent, proposed work, implementation sequences, or evidence at particular revisions.
They are not current installation or usage instructions.
Use the [audience guides](../README.md) for supported procedures and the [decision log](../decisions/README.md) for decision status.
An accepted specification or decision does not establish that every feature has shipped.

## Product and architecture specifications

- [First-iteration specification](first-iteration.md): accepted specification, preserved as written; subsequent decisions record changes.
- [Original implementation plan](../implementation-plan.md): historical OpenCode/ACP delivery plan.
- [Runtime boundary contracts](2026-10-07-runtime-boundary-contracts.md).
- [Docker execution boundary](2026-10-07-docker-execution-boundary.md).
- [Location-independent workspace evidence](2026-10-08-location-independent-workspaces.md).
- [Retained-state bindings](2026-10-08-retained-state-bindings.md).
- [Repository environments](2026-10-08-repository-environments.md).

## Pi and backend delivery records

These capture the runtime transition and its qualification work; current deployment uses the clean Pi-only baseline.

- [Pi and pluggable backends delivery sequence](2026-10-05-pi-and-pluggable-execution-backends.md).
- [Microsandbox implementation and qualification](2026-10-09-microsandbox-backend.md).
- [Pi protocol adapter](2026-10-09-pi-protocol-adapter.md).
- [Pi configuration and authenticated MCP](2026-10-09-pi-configuration-and-mcp.md).
- [Durable Pi Sessions and History Replay](2026-10-09-durable-pi-sessions.md).
- [Complete Pi Agent Turns](2026-10-09-complete-pi-agent-turns.md).
- [Failure recovery](2026-10-10-pi-failure-recovery.md).
- [Upgrades and state recovery](2026-10-10-pi-upgrades-and-state-recovery.md).
- [Operational integration and backend CI](2026-10-11-pi-operational-integration.md).
- [Deployment documentation and cutover](2026-10-11-pi-operator-documentation-and-cutover.md).
- [Pi-only final qualification](2026-10-13-pi-only-final-qualification.md).
- [Clean supported release baseline](2026-10-16-clean-runtime-baseline.md).
- [Development image delivery](2026-10-16-development-image.md).
- [Participant compatibility cleanup](2026-10-17-participant-compatibility-cleanup.md).
- [Retired runtime experiments](2026-10-17-retire-runtime-experiments.md).
- [Superseded runtime history](runtime-history/README.md).
- [Historical OpenCode-to-Pi migration](../migrations/opencode-to-pi.md).

## Focused designs and plans

Consult each document and linked Work Item for its own delivery/evidence status.

- [Instruction ownership and handoffs](2026-10-04-instruction-ownership-and-handoffs.md).
- [Repository skills and Profiles](2026-10-04-repository-skills-and-agent-profiles.md).
- [Verified multi-commit publication](2026-09-28-verified-multi-commit-publication-design.md).
- [Pi startup readiness budget](2026-10-12-pi-startup-readiness-budget.md).
- [Terminal corroboration recovery](terminal-corroboration-recovery-plan.md).
- [MCP failure diagnostics](mcp-failure-diagnostics-improvement.md).
- [Agent race toolchain](agent-runtime-race-toolchain.md).
- [Application observability](application-observability-first-release.md).
- [Documentation cleanup](2026-10-18-documentation-cleanup.md).

[Documentation hub](../README.md)

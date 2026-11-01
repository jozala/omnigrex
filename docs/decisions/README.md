# Project decision log

Record consequential product, engineering, operations, Workflow, and architectural decisions here.
Use the [decision-record skill](../../.agents/skills/omnigrex-decision-records/SKILL.md) for the writing process and canonical template.
An accepted decision is not a claim that its implementation has shipped; follow its linked plan or issue for delivery status.

## Decisions

| Record | Status | Date |
| --- | --- | --- |
| [0001: Keep a project-wide decision log](0001-keep-a-project-wide-decision-log.md) | Accepted | 2026-10-04 |
| [0002: Use current-deployment agent instructions](0002-use-current-deployment-agent-instructions.md) | Accepted | 2026-10-04 |
| [0003: Separate personality from platform and Workflow guidance](0003-separate-personality-from-platform-and-workflow-guidance.md) | Accepted | 2026-10-04 |
| [0004: Publish handoff summaries before continuing](0004-publish-handoff-summaries-before-continuing.md) | Accepted | 2026-10-04 |
| [0005: Separate execution backends from coordination](0005-separate-execution-backends-from-coordination.md) | Accepted | 2026-10-07 |
| [0006: Use Pi through an agent-specific adapter](0006-use-pi-through-an-agent-specific-adapter.md) | Accepted | 2026-10-07 |
| [0007: Supply provider API keys through disposable files](0007-supply-provider-api-keys-through-disposable-files.md) | Accepted | 2026-10-07 |
| [0008: Keep workspace capture separate from publication policy](0008-keep-workspace-capture-separate-from-publication-policy.md) | Accepted | 2026-10-07 |
| [0009: Pin Participant images and use working-revision tools](0009-pin-participant-images-and-use-working-revision-tools.md) | Accepted | 2026-10-07 |
| [0010: Seal portable workspace evidence before publication](0010-seal-portable-workspace-evidence-before-publication.md) | Accepted | 2026-10-08 |
| [0011: Bind retained state and validate offline Session archives](0011-bind-retained-state-and-validate-offline-session-archives.md) | Accepted | 2026-10-08 |
| [0012: Qualify repository environments without rebinding Sessions](0012-qualify-repository-environments-without-rebinding-sessions.md) | Accepted | 2026-10-08 |
| [0013: Use the Microsandbox Go SDK and POSIX-faithful local bindings](0013-use-the-microsandbox-go-sdk-and-posix-bindings.md) | Accepted | 2026-10-09 |
| [0014: Own Pi resource loading with a pinned runner](0014-own-pi-resource-loading-with-a-pinned-runner.md) | Accepted | 2026-10-09 |
| [0015: Reserve exact Pi Sessions and replay bounded append-order history](0015-reserve-exact-pi-sessions-and-replay-bounded-history.md) | Accepted | 2026-10-09 |
| [0016: Qualify Pi Agent Turns with existing settlement policy](0016-qualify-pi-turns-with-existing-settlement-policy.md) | Accepted | 2026-10-10 |
| [0017: Bind Pi qualification to backend contracts](0017-bind-pi-qualification-to-backend-contracts.md) | Accepted | 2026-10-10 |
| [0018: Gate Pi publication on both backend deployments](0018-gate-pi-publication-on-both-backend-deployments.md) | Superseded by 0020; publication gates retained | 2026-10-11 |
| [0019: Separate Pi startup readiness from ordinary RPC deadlines](0019-separate-pi-startup-readiness-from-rpc-deadlines.md) | Proposed | 2026-10-12 |
| [0020: Finish Pi-only runtime selection and neutral outcome evidence](0020-finish-pi-only-runtime-and-neutral-outcome-evidence.md) | Accepted | 2026-10-13 |
| [0021: Activate from the current Change Proposal](0021-activate-from-current-change-proposal.md) | Accepted | 2026-10-04 |
| [0022: Track heads without resuming work](0022-track-heads-without-resuming-work.md) | Accepted | 2026-10-07 |
| [0023: Expose scoped CI failure diagnostics with orchestrator-owned credentials](0023-scoped-ci-failure-diagnostics.md) | Proposed | 2026-10-06 |
| [0024: Establish a clean supported release baseline](0024-establish-a-clean-supported-release-baseline.md) | Accepted | 2026-10-16 |
| [0025: Require current Docker management ownership](0025-require-current-docker-management-ownership.md) | Accepted | 2026-10-17 |
| [0026: Use an image-local Go module source for the development environment](0026-use-an-image-local-go-module-source.md) | Accepted | 2026-10-16 |
| [0027: Use native guest scratch for development Go tests](0027-use-native-scratch-for-development-go-tests.md) | Accepted | 2026-10-16 |
| [0028: Keep an empty domain parent for guest workloads and nested containers](0028-keep-a-domain-parent-for-nested-containers.md) | Accepted | 2026-10-16 |
| [0029: Preserve historical PID budgets and size new Pi Participants for development](0029-preserve-historical-pid-budgets-and-size-new-participants.md) | Accepted | 2026-10-16 |
| [0030: Cache immutable registry documents for offline verification](0030-cache-immutable-registry-documents-for-offline-verification.md) | Accepted | 2026-10-18 |
| [0031: Organize documentation around reader responsibilities](0031-organize-documentation-around-reader-responsibilities.md) | Accepted | 2026-10-18 |
| [0032: Own ChatGPT subscription credentials in the controller](0032-own-chatgpt-subscription-credentials-in-the-controller.md) | Accepted | 2026-10-20 |
| [0033: Resolve controller packages from signed distribution repositories](0033-resolve-controller-packages-from-signed-alpine-repositories.md) | Accepted | 2026-10-20 |
| [0034: Preserve Microsandbox failure evidence without changing cleanup policy](0034-microsandbox-diagnostics.md) | Accepted | 2026-10-20 |
| [0036: Aggregate independent CI qualification behind the existing required check](0036-aggregate-parallel-ci-qualification.md) | Accepted | 2026-10-21 |
| [0037: Reuse PostgreSQL test processes while isolating databases](0037-reuse-postgres-processes-with-isolated-test-databases.md) | Accepted | 2026-10-21 |
| [0038: Derive publication read targets without rebinding Turn authority](0038-derive-publication-read-targets-from-the-turn-ledger.md) | Accepted | 2026-10-24 |
| [0039: Project Pi events at the owned RPC subscription](0039-project-pi-control-events.md) | Accepted | 2026-10-25 |
| [0040: Use native Pi compaction within the existing Turn lifecycle](0040-use-native-pi-compaction-within-turn-lifecycle.md) | Accepted | 2026-10-25 |
| [0041: Separate Pi compaction and retry timestamps](0041-separate-pi-compaction-and-retry-timestamps.md) | Accepted | 2026-10-26 |
| [0042: Associate publication at Turn settlement without granting readiness](0042-associate-publication-at-turn-settlement.md) | Accepted | 2026-10-26 |
| [0043: Coordinate Microsandbox payload and catalog upgrades](0043-coordinate-microsandbox-catalog-upgrades.md) | Accepted | 2026-10-25 |
| [0044: Retain bounded provider-failure observations](0044-retain-bounded-provider-failure-observations.md) | Accepted | 2026-10-25 |
| [0045: Enforce hidden per-Turn model-iteration ceilings](0045-enforce-hidden-per-turn-step-budgets.md) | Accepted | 2026-10-24 |
| [0046: Classify telemetry failures without error prose](0046-classify-telemetry-failures-without-error-prose.md) | Accepted | 2026-10-28 |
| [0047: Verify CI read provenance independently of review authority](0047-verify-ci-read-provenance.md) | Accepted | 2026-10-27 |
| [0048: Retain Participant-local Human Handoff provenance](0048-retain-participant-handoff-provenance.md) | Accepted | 2026-10-27 |
| [0049: Gate Reviewer admission on CI completion](0049-gate-reviewer-admission-on-ci-completion.md) | Accepted | 2026-10-27 |
| [0050: Deliver default-branch snapshots as additive Git data](0050-deliver-default-branch-snapshots-as-additive-git-data.md) | Accepted | 2026-10-27 |
| [0051: Exclude execution cleanup without locking Turn leases](0051-exclude-execution-cleanup-without-locking-turn-leases.md) | Accepted | 2026-10-28 |
| [0052: Carry current Human Handoff publication across tracking revisions](0052-carry-current-handoff-publication-across-tracking-revisions.md) | Accepted | 2026-10-28 |
| [0053: Export live budget observations in the controller](0053-export-live-budget-observations-in-the-controller.md) | Accepted | 2026-10-27 |
| [0054: Designate manual pre-1.0 product releases](0054-manual-product-releases.md) | Accepted | 2026-10-30 |
| [0055: Serialize Work Item clarification at Turn boundaries](0055-serialize-work-item-clarification-at-turn-boundaries.md) | Accepted | 2026-11-01 |
| [0056: Bound collaboration prompt projections without truncating history](0056-bound-collaboration-prompt-projections.md) | Accepted | 2026-11-01 |

Records 0021-0023 retain the decisions independently numbered 0005-0007 on `main` while the Pi records were developed.
Their original filename links remain available as pointers to the uniquely numbered canonical records.
Record 0050 similarly retains the default-branch delivery decision independently numbered 0048 before the Participant provenance decision merged.
Record 0051 retains the installation-fencing decision independently numbered 0049 before the Reviewer admission decision merged.
Record 0052 retains the Human Handoff publication decision independently numbered 0049 before the Reviewer admission decision merged.

Record 0053 retains the budget observation decision independently numbered 0046 before the telemetry failure diagnostic decision merged.

## Existing architectural decisions

ADR identifiers remain stable; the links below locate their authoritative current or archived records.
New decisions use this log.
Do not copy an existing ADR into a second record.

- [ADR 0001: Separate visible Workflow and execution state](../adr/0001-separate-visible-workflow-and-execution-state.md)
- [ADR 0002: Runtime protocol boundary](../plans/runtime-history/adr-0002.md) - Superseded by [decision 0006](0006-use-pi-through-an-agent-specific-adapter.md).
- [ADR 0003: Orchestrator owns session control](../adr/0003-orchestrator-owns-session-control.md)
- [ADR 0004: Mediate agent capabilities through MCP](../adr/0004-mediate-agent-capabilities-through-mcp.md)
- [ADR 0005: Use separate Developer and Reviewer GitHub Apps](../adr/0005-use-separate-developer-and-reviewer-github-apps.md)
- [ADR 0006: Control Docker directly from the orchestrator](../adr/0006-control-docker-directly-from-the-orchestrator.md) - Superseded by [decision 0005](0005-separate-execution-backends-from-coordination.md).
- [ADR 0007: Use code-defined Workflow Definitions and durable Attempt Stages](../adr/0007-use-code-defined-workflow-definitions-and-durable-attempt-stages.md)
- [ADR 0008: Separate Stage Assignments from Agent Participants](../adr/0008-separate-stage-assignments-from-agent-participants.md)
- [ADR 0009: Select GitHub App credentials by operation](../adr/0009-select-github-app-credentials-by-operation.md)
- [ADR 0010: Use one provider credential bundle before proxy mediation](../plans/runtime-history/adr-0010.md) - Superseded by [decision 0007](0007-supply-provider-api-keys-through-disposable-files.md).
- [ADR 0011: Discover repository Agent Profiles and select separately](../adr/0011-discover-repository-agent-profiles-and-select-separately.md)
- [ADR 0012: Load repository skills from the current checkout](../adr/0012-load-repository-skills-from-the-current-checkout.md)

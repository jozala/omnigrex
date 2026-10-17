# Consolidate Participant identity and current Docker ownership

This completes the compatibility and domain consistency pass requested after [retiring the runtime experiments](2026-10-17-retire-runtime-experiments.md).
The fresh, unpublished schema baseline follows [decision 0024](../decisions/0024-establish-a-clean-supported-release-baseline.md).

## Required outcomes

- Use one Participant identity in Store records, preparation results, and the baseline schema.
- Require preparation for the exact Workflow Stage through `Stages`, without Developer/Reviewer compatibility inputs.
- Remove the unused Session getter alias, obsolete installation column, alias view, and null fallbacks for mandatory binding fields.
- Keep current actor attribution, Stage Assignment immutability, Session ownership, fencing, captures, retention, and future Pi upgrade qualification.
- Require current Docker Runtime Process management ownership for inventory and cleanup, retaining separate diagnostic ownership/expiry and exact-ID orphan revalidation.

The schema uses the physical `agent_participants` table and `agent_participant_id` foreign keys, with one `AgentParticipantID` in Store records and coordinator-owned payloads.
Stage Assignments and Assignment Generations retain their distinct domain meanings.
Backend resource labels, retained-state paths, and external GitHub artifact marker encodings keep their existing exact identities.
[Decision 0025](../decisions/0025-require-current-docker-management-ownership.md) records the management-marker requirement.

## Verification

The initial regressions failed because Participants were still represented by a table plus alias view, missing exact-Stage preparation fell back to a built-in Role preparation, and complete unmarked Docker identities were inventoried and removed.
Those regressions pass with the consolidated implementation.

Passed against the final code:

| Environment | Check | Result |
| --- | --- | --- |
| macOS/arm64 | `mise run check` | Formatting, vet, production reachability, race-enabled units, and Compose validation passed. |
| macOS/arm64 with Docker Desktop | Focused Store preparation, runtime contract, Session, and canonical-schema integration tests | Passed. |
| macOS/arm64 with Docker Desktop | `mise run test-docker-integration` | Full shared integration, Pi protocol, controlled Workflows, failure recovery, source-to-candidate upgrade/replay, image-only deployment, and coordinated restore passed. |
| Linux/amd64 with Docker and KVM | `mise run check` | Passed. |
| Linux/amd64 with Docker and KVM | `OMNIGREX_MICROSANDBOX_KVM_GID=<host-kvm-group> mise run test-microsandbox` | Full native conformance, capture/storage, nested Docker/Compose/Testcontainers, Pi protocol, controlled Workflows, failure recovery, upgrade/replay, deployment, and coordinated restore passed. |
| Docker and Microsandbox | `OMNIGREX_PI_LIVE_TEST_PATTERN='^TestPiProviderSmokeBootstrap$' mise run test-pi-live` with explicit fixture-key input | Both providers passed production golden-image startup, exact configuration, authenticated MCP discovery, durable Session creation, and stopped-state credential auditing without provider requests. |
| Working changes | `git diff --check` | Passed. |

The Linux native aggregate and provider-smoke bootstrap completed with exit status 0.
The relocated nested-container fixtures and the native provider-smoke harness from the preceding cleanup are now exercised on actual KVM, resolving its earlier host-connectivity blocker.
The private verification source was checksum-matched to the working changes.
Real-provider prompting, release publication, and operator deployment are separate evidence and were not performed by this cleanup.

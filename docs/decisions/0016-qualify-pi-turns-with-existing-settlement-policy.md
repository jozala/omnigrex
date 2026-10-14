---
status: accepted
date: 2026-10-10
decision-makers: [OpenCode implementation agent]
---
# Qualify Pi Agent Turns with existing settlement policy

## Context

Step 12 must connect the qualified Pi configuration and durable Session layer to complete Agent Turns while preserving the existing OpenCode deployment through the preparation sequence.
The execution Worker and deferred corroboration records currently use an ACP-named ending representation even though its values encode platform settlement policy.
Backend selection must not select a different Workflow decision or publication rule.

## Decision

Expose a neutral Turn adapter and connection at the launch boundary, selected by composition using the immutable Runtime Profile reference.
Pi owns process compilation, configuration translation, and Session operations; the common Launcher retains backend, workspace, MCP authority, stop, capture, and cleanup ownership.

Register the candidate `pi-rpc/v1` Runtime Profile only when both optional digest-pinned image and platform settings are supplied.
Qualification uses fresh isolated Work Items and storage with explicit Pi Profiles.
Existing Participant bindings are not rewritten when candidate settings or repository preferences change.

Use a narrow temporary translation from neutral prompt outcomes to the existing durable ending vocabulary before settlement.
Preserve explicit token-limit and cancellation outcomes rather than translating every settled Pi response into a normal ending.
Transport loss remains uncertain prompt execution, provider failure remains a failure, and completed MCP mutations are still reconciled under the existing terminal-intent policy only after the environment stop/capture barrier.
Remove the ACP-named compatibility surface during Step 17 rather than changing the durable ending policy while proving Pi's vertical integration.

## Rationale

Duplicating the execution Worker would split admission, credentials, mutation recovery, and settlement policy between runtimes.
Putting Pi protocol or execution-backend switches in the Workflow reducer would violate the accepted ownership boundary.
Replacing all historical ending records during preparatory wiring would conflate Agent Turn integration with the later clean cutover.
An explicit neutral translation preserves existing observable policy and makes its temporary ownership clear.

## Consequences

Docker and Microsandbox run the same preparation, prompting, publication, review, and settlement code.
The Pi golden environment qualifies owned resources against embedded checksums and compares installed SDK JavaScript with the locked source distribution rather than accepting a version claim alone.
The controlled Workflow gate substitutes only disposable provider/GitHub fixtures and fixture image addressing, while retaining production coordination and publication checks.
Operational cutover, final backend-selected doctor behavior, published releases, and Pi upgrade qualification remain later milestones.

## Links

- [Step 12 implementation and evidence](../plans/2026-10-09-complete-pi-agent-turns.md).
- [Agent-specific adapter decision](0006-use-pi-through-an-agent-specific-adapter.md).
- [Pinned Participant image policy](0009-pin-participant-images-and-use-working-revision-tools.md).

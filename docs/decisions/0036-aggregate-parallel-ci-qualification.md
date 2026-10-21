---
status: accepted
date: 2026-10-21
decision-makers: [OpenCode implementation agent, delegated by Mariusz]
---
# Aggregate independent CI qualification behind the existing required check

## Context

The CI `check` job serialized development-image build, module-source checks, and a real-image unit suite after the Docker checks, while Microsandbox qualification already ran independently.
The development unit suite can use a separate runner and does not need the Docker check's artifacts or state.
The existing `check` context must remain compatible with required-check configuration, and publication must remain blocked when any of these qualifications fails or is skipped.

## Decision

Run Docker checks, development-image qualification, and Microsandbox qualification as independent jobs.
Retain `check` as an aggregate required job that runs after all three and succeeds only if each result is `success`.
Make publication depend on that aggregate; keep exact published-digest deployment and development-environment smoke after publication.
Build the development job's golden image from the same checkout and platform, then run the existing module-source and resource-limited unit-suite scripts without changing their behavior.

## Rationale

This places independent work on separate runners and removes the development suite from the serialized Docker path.
Making the development job depend on Docker checks would preserve the delay without sharing a necessary artifact.
Making publication depend on a new job while leaving `check` as the Docker job would let existing branch protection pass a Change Proposal whose development qualification failed.
The aggregate also rejects skipped or cancelled prerequisites rather than treating them as success.

## Consequences

CI uses another runner and rebuilds its own same-commit golden image.
The required `check` context finishes only after the slowest prerequisite; its result and the publication gate both cover all three qualifications.
Exact-digest release qualification remains after publication, separate from the source-built prepublication checks.

## Links

- [Contributor testing and release evidence](../contributors/testing.md#release-evidence).
- [Image-local Go module source](0026-use-an-image-local-go-module-source.md).
- [Both-backend publication gates](0018-gate-pi-publication-on-both-backend-deployments.md).

---
status: accepted
date: 2026-10-27
decision-makers: [Mariusz]
---
# Gate Reviewer admission on CI completion, not success

## Context

A Reviewer spending an Agent Turn waiting for remote CI consumes execution capacity and inference without improving the review.
The product policy in [Work Item 121](https://github.com/jozala/omnigrex/issues/121) was agreed with Mariusz on 2026-10-25.
Acceptance of that design is not evidence of implementation or backend qualification.

## Decision

Default newly admitted Reviewer Turns to durable controller-side `after_ci` scheduling.
Repository owners may explicitly select `immediate`, expected CI selectors, timeout, and registration grace through a versioned policy read from a trusted default-branch revision.
Freeze the logical request's policy and inventory; infrastructure retries retain its absolute deadline while refreshing observations.
A waiting head or verified test-merge replacement invalidates old evidence and restarts only registration grace, bounded by that deadline.

Use the shared inventory, observation, and verified-target contract from decision 0047, retaining decision 0038's separation of read provenance from mutation authority.
The shared contract now reports pending mergeability as an explicit mapping failure without granting alternate-SHA access, and includes result timestamps and producer/suite identity in matched results.
Terminal non-success releases the wait just as terminal success does.
Incomplete inventory discovery is retried under the original deadline until a complete snapshot can be frozen.
Unavailable discovery is not empty policy; missing, pending, and unresolved evidence waits until completion or explicit incomplete-evidence timeout admission.
Keep the code-defined Stage graph and existing Review Cycle and infrastructure budgets unchanged.

## Rationale

Starting an Agent to poll CI does not provide useful isolation or durability.
Requiring CI to pass would conflate scheduling with review qualification and could prevent a Reviewer from investigating failures.
Reading policy from the PR head would let the Change Proposal redefine its own admission gate.
Freezing unrelated Profiles or automatically merging the target branch would introduce authority and publication changes unrelated to scheduling.

## Consequences

The controller must fence request observations, coalesce waiting synchronizations, and allocate at most one successor Turn per admission intent.
GitHub presentation uses a separate waiting label and one editable progress comment per location, not a self-gating check run.
Admitted review context is a point-in-time observation, never an approval certificate.
Avoiding idle Agent polling remains a repository-overridable instruction default subject to operator precedence; prose cannot grant capabilities or alter scheduling.

## Links

- [Verified CI provenance](0047-verify-ci-read-provenance.md).
- [Publication-derived reads](0038-derive-publication-read-targets-from-the-turn-ledger.md).
- [Code-defined Workflow](../adr/0007-use-code-defined-workflow-definitions-and-durable-attempt-stages.md).
- [Implementation and qualification plan](../plans/ci-gated-review-admission.md).

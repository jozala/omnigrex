---
status: accepted
date: 2026-10-27
decision-makers: [Omnigrex implementation agent]
---
# Verify CI read provenance independently of review authority

## Context

GitHub can report required CI on a distinct PR test-merge commit, while a workflow checking out a merge ref can still report the PR head through its API.
Following arbitrary Agent-provided SHAs, resource IDs or the current branch tip would broaden the authority established by a frozen Agent Turn.
Requiredness, availability, completion and success also answer different diagnostic questions.

## Decision

Extend the existing CI tools with a shared inventory, observation and pure matching contract.
Discover current classic protection and effective active branch rules independently of result observation.
Accept an explicit provider no-classic-protection response on a readable branch as absence, but never infer absence from a generic inaccessible 404.
Preserve every source and App constraint; unsupported workflows and unknown rule types remain unresolved.
Legacy statuses do not prove an integration App identity.
Select the newest resource per context/producer without turning terminal non-success into success.

Verify the current repository identity for every CI tool path, including head-only reads, rather than treating a reusable repository name as its immutable ID.
Derive additional CI read access only after verifying the same PR identity and reviewed head, its observed base repository and branch, the current base tip, and the test-merge commit's ordered base/head parents.
Reobserve the PR mapping before granting access and repeat verification for each diagnostic request.
Reject queue/group Actions sources and inconsistent repository, parent, attempt or continuation metadata.
Keep API-reported resource commits unchanged, including resources that report the PR head despite executing a merge checkout.

## Rationale

This extends the publication-derived read boundary from decision 0038 instead of replacing the Turn's immutable scope.
Trusting only `merge_commit_sha` would miss stale base mappings and merged-PR semantics.
Treating a combined status or a successful workflow as complete required evidence would hide collisions, missing results and provider attribution gaps.
Controller waiting and selection policy remain separate work.

## Consequences

Read responses identify the reviewed head, actual queried commit, mapping observation time and failures.
Changed or unavailable mappings grant no alternative access; historical response provenance remains identifiable, but an invalidated mapping cannot authorize a new diagnostic request.
Evidence continuations reject changed policy/results rather than silently splicing snapshots.
Developer App Administration and Commit statuses read permissions are required; Reviewer-native mutation authority is unchanged.
Neither successful discovery nor this decision asserts that CI passed or that a Change Proposal is ready.

## Links

- [Work Item 122](https://github.com/jozala/omnigrex/issues/122).
- [Publication read targets, decision 0038](0038-derive-publication-read-targets-from-the-turn-ledger.md).
- [Work Item 118](https://github.com/jozala/omnigrex/pull/118).
- [Follow-up scheduling Work Item 121](https://github.com/jozala/omnigrex/issues/121).

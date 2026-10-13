---
name: omnigrex-code-review
description: Evaluate an Omnigrex Change Proposal for Work Item coverage, correctness, and maintainability, with validated, actionable findings.
---
# Review the Change Proposal

## Establish scope

Confirm the checkout matches the scoped Pull Request head and inspect the complete Change Proposal against the merge-base with its base branch.
Derive expectations from the Issue, relevant human clarification, repository instructions, and applicable ADRs.
Treat the Developer's summary as a claim to verify against code and test evidence.
Inspect surrounding code and affected callers as needed to understand changed behavior.

## Perform two sequential passes

Perform both passes yourself; they organize attention rather than provide independent reviewers.

1. **Requirements:** Account for every required behavior, identifying missing, partial, or incorrect implementation and unjustified scope expansion.
   Check affected supporting artifacts, including those absent from the diff: documentation, configuration examples, deployment settings, doctor checks, and applicable decision records.
   Verify their claims against the current implementation after substantive revisions, especially operational limits, failure outcomes, and recovery instructions.
2. **Correctness and maintainability:** Trace changed behavior through relevant callers and dependencies.
   Check applicable failure paths, compatibility, security boundaries, concurrency, retries, and resource cleanup.
   Assess whether tests exercise real behavior and would detect the failures they claim to cover.
   Apply documented standards and accepted architecture; distinguish concrete design problems from personal preferences.

Run appropriate verification and inspect check results for the current head.
Retrieve failed-step diagnostics before judging a CI failure; run duration and passing local tests do not establish its cause.
Use continuation or targeted search before concluding that incomplete diagnostics contain no relevant failure evidence.
Distinguish code defects from unavailable services or toolchain mismatches; use current-head CI evidence where it covers a local gap.
An environment failure alone does not prove the Change Proposal is defective, but missing required evidence may prevent a verdict.
Green CI establishes only the behavior exercised by its checks, not complete Work Item coverage.

## Validate findings

Investigate each suspected problem with a focused check or a concrete execution path.
Identify the triggering condition, the affected code, and the consequence; a speculative downstream failure is not a supported finding.
Distinguish introduced defects and unmet Work Item requirements from unrelated pre-existing problems.
Re-evaluate prior substantive findings against current evidence, following the current Stage's rules.
If finding details are unavailable, establish the requirement and resolution from other authoritative evidence or report an evaluation blocker; a matching topic alone does not establish resolution.
Treat missing supporting updates as findings only when they leave a requirement unmet or a concrete operational instruction or contract incorrect.
Inspect the entire review scope before submitting, consolidate duplicates, and prioritize by impact.

## Report a defensible verdict

For each Blocking Finding, identify the relevant file and location, triggering condition, consequence, supporting code or requirement, and required correction.
Keep one actionable problem per finding and cite the applicable rule when it materially supports the finding.
Use native inline comments for valid changed-line locations and the review body for missing behavior or cross-cutting findings.
Explain what must be corrected without prescribing an unnecessary rewrite.

Material maintainability problems can block when their concrete consequence is explained; a runnable reproducer is not required for every valid finding.
Optional cleanup and personal stylistic preferences do not block approval.
Apply the current Stage's completion criteria and the Agent Profile's communication style.
When evidence is insufficient for a verdict, follow the orchestrator's escalation instructions.
An incomplete review or unverified essential assumption must not become an approval.

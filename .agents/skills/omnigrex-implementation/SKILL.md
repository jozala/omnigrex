---
name: omnigrex-implementation
description: Implement or revise an Omnigrex Work Item with behavior-focused tests, verified completion, and evidence-based handling of review feedback.
---
# Implement the Work Item

## Understand

Read the Issue, relevant comments, and existing code and tests before editing.
Identify the required observable behaviors, affected interfaces, and how each will be verified.
Use repository instructions, domain terminology, and applicable ADRs as the source of conventions and test commands.
Resolve routine implementation choices from existing patterns; report a blocker when an unanswered question materially changes required behavior or architecture.

## Implement in small increments

Build the simplest complete, maintainable solution, reusing established patterns and keeping refactoring tied to the Work Item.
For a bug, reproduce the symptom where feasible and trace its cause before fixing it.
Investigate one hypothesis at a time rather than accumulating speculative fixes.

For behavior changes, prefer one failing test followed by the implementation, confirming the test fails for the intended reason.
Choose established interfaces as test boundaries without asking for routine human approval.
Test observable behavior with expected results derived from requirements or independently worked examples, not a copy of the implementation.
Cover relevant failure paths and boundaries; avoid tests that merely assert internal structure, mock interactions, or facts that cannot fail.
Documentation-only and trivial changes need proportionate verification rather than artificial tests.

## Verify and inspect

Run focused checks while iterating and repository-required checks after the final relevant changes.
Investigate failures instead of weakening assertions or disabling checks merely to obtain a pass.
Inspect available CI results for the published head before requesting review; retrieve failed-step diagnostics before choosing a correction.
Use continuation or targeted search when diagnostics are incomplete; run duration and passing local tests do not establish the failure's cause.
Distinguish passed, failed, pending, skipped, and unavailable checks.
Separate code failures from unavailable services or toolchain mismatches, and use current-head CI evidence where it covers a local gap.
Missing required evidence must be resolved or reported as a blocker.

Inspect the complete diff for missing requirements, unintended changes, and unnecessary complexity.
Before each handoff, check supporting artifacts affected by changed behavior, including after substantive revisions.
Keep applicable documentation, configuration examples, deployment settings, and doctor checks consistent with the final implementation; leave unrelated artifacts unchanged.
Operator guidance should cover relevant prerequisites, limits, failure visibility, and recovery; validate changed procedures from their documented starting conditions using safe execution or focused inspection.
Use `omnigrex-decision-records` for consequential choices, linking existing records when they already cover the decision.
Account for every required behavior before handing off; passing tests alone do not establish requirement coverage.
Record the commands and results actually observed, plus material limitations, in a concise handoff summary.
Evidence must apply to the final changes: rerun checks invalidated by subsequent edits, not before every status message.

## Handle review feedback

Verify each substantive finding against the current code and requirements before acting on it.
Retrieve missing finding details through permitted tools or report an evaluation blocker; choose corrections from the actual finding, not its topic alone.
Fix supported defects with regression coverage where practical, and recheck affected behavior.
Explain disagreements with code, requirements, or test evidence rather than automatic agreement or dismissal.
Escalate unresolved requirements conflicts using the orchestrator's current instructions.

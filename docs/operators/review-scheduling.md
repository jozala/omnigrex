# Operate controller-side review scheduling

[Operator guide](README.md) · [Repository policy reference](../reference/repository-configuration.md#review-scheduling)

## Prerequisites and limits

Use a release qualified for both the Docker and Linux/KVM controlled Workflow gates.
Apply its forward-only PostgreSQL migrations with the normal controller startup procedure; old completed Turns and existing Human Handoffs are not reactivated or rewritten.
The shared CI reader's permissions and strict App preflight requirements are authoritative in [GitHub Apps](github-apps.md#developer-app).
No Checks write permission or CI-completion webhook subscription is added.

Repository owners control scheduling within controller limits: timeout 1 second..2 hours and registration grace 0..5 minutes, with grace no longer than timeout and whole-second precision.
The defaults are 30 minutes and 60 seconds.
Policy is structured configuration, not Agent Profile prose, and does not grant execution capabilities or alter the Workflow graph.

## Recognize healthy waiting

The Workflow stays in its review Stage with the separate `omnigrex:waiting-for-ci` label on the Work Item and current PR.
A controller-owned editable progress comment identifies the reviewed head, deadline, expected/observed evidence, and release reason.
Waiting allocates no Reviewer Turn, execution job, Runtime Process, or active execution slot.
An existing persistent Reviewer Session may remain retained.

The preparation job is delayed between bounded observations and releases its controller lease.
Healthy waits preserve the remaining finite failure allowance by extending claim allowance while retaining monotonic audited claim numbers.
They do not increment Workflow infrastructure failures, Review Cycles, or Agent step usage.
Controller restart reclaims leases and resumes durable delayed observations rather than requiring an event or an Agent to poll.

## Investigate a wait or release

Check the deadline and progress comment before treating review-stage inactivity as a stuck Runtime Process.
A missing expected check remains missing after registration grace; a provider error or unsupported required workflow remains explicit rather than proving empty requirements.
Incomplete discovery is retried until a complete inventory is selected or the original deadline releases with incomplete evidence.
The selected inventory is frozen, so newly added required checks affect new requests rather than an existing wait.
A PR-head or verified merge-target replacement invalidates old evidence and restarts grace without extending the original deadline.

Completion includes failed or cancelled results.
Timeout admits review with incomplete-evidence context; it is not an infrastructure-failure exhaustion or a passing qualification.
The comment distinguishes review queued from actual Reviewer Turn allocation and never certifies approval or mergeability.
Detailed logs and diagnostics remain behind the existing scoped CI tools.

If publication of a waiting update races admission, cancellation, or closure, the serialized revision-aware visible-effect queue reconciles the latest state on retry.
Do not delete durable requests, edit job counters, blindly recreate comments, or reapply `omnigrex:run` merely to make a CI notification take effect.
A lost create response is recovered by the stable workflow marker, and meaningful updates edit that comment rather than publishing one per observation.

Closing the Issue, superseding an Attempt, Human Handoff, or transfer to human Session control invalidates the pending gate.
Later CI observations cannot resume stopped automation.
Use the existing [human reactivation procedure](../users/workflow-and-labels.md#start-or-reactivate-work) when autonomous work should resume, creating a fresh policy snapshot and deadline.
Do not merge or rebase a published branch to manufacture CI as part of scheduling recovery.

## Instruction defaults

Developers may hand off with remote CI pending after appropriate local verification and truthful reporting.
Reviewers normally perform useful review after immediate, no-CI, or timeout admission instead of rebuilding a controller wait in their Turn.
This polling preference is expressly repository-overridable unless explicit operator guidance forbids it.
The [instruction hierarchy](agent-policy.md) and essential-evidence obligations remain unchanged.

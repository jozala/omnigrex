---
status: accepted
date: 2026-10-20
decision-makers: [Mariusz]
---
# Preserve Microsandbox failure evidence without changing cleanup policy

## Context

A Reviewer completed its work on Change Proposal 105, but native cleanup briefly failed with only the SDK error kind visible.
SDK source details were intentionally removed from public errors, and deleting the sandbox also deleted its native log directory.
Operators need the original failure cause and native evidence after disposable environments disappear.

## Decision

Log original SDK failures without redaction through the orchestrator's existing JSON operator logger, identifying the actual SDK operation and available execution/resource context.
Keep returned errors sanitized so this diagnostic policy does not expose raw details through GitHub or other existing consumers.
Add a TODO at this logging boundary to prevent sensitive-data logging in the future.

Preserve complete available `runtime.log`, `kernel.log`, `exec.log`, and numeric rotations in host-only Participant storage before authorized native removal.
Do not forward their contents to Grafana or include them in supplemental Session exports.
Use existing Participant retention rather than another expiry policy.
Copying is best-effort and bounded; failed preservation warns but does not block cleanup or establish stop/writeback evidence.
Mariusz requested unredacted diagnostics and all three files, then approved implementation of the plan including its retention and best-effort choices.

## Rationale

Retaining only error kinds loses the cause needed to diagnose SDK failures.
Returning raw errors to all callers would unnecessarily broaden exposure.
Native severity filtering would discard kernel/primary-execution evidence and depend on unstructured formats; complete files preserve what is available.
A second collector/retention service adds coordination without improving the existing Participant lifecycle.
Failing cleanup solely to protect logs would let optional evidence prolong disposable resources.

## Consequences

Operator logs and local archives can contain sensitive data and need restricted access and inspection before sharing.
Rotation, timeouts, failed-create rollback, and removal-time writes can leave evidence incomplete.
This is not a guarantee of lossless live logging or deployment qualification.

## Links

- [Failure that motivated the investigation](https://github.com/jozala/omnigrex/pull/105#issuecomment-6287892704)
- [Operator diagnostics](../operators/troubleshooting.md#retained-microsandbox-native-logs)
- [Retained-state authorization](0011-bind-retained-state-and-validate-offline-session-archives.md)

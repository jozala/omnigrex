# Workflow and labels

[User guide](README.md) · [Documentation hub](../README.md)

## Read the labels

| Label | Meaning |
| --- | --- |
| `omnigrex:run` | Human command to create or reactivate a direct-development Workflow Attempt |
| `omnigrex:plan` | Authorized human command to start planning on an open, unstarted Work Item |
| `omnigrex:planning` | Planner work is active, pending, or waiting for discussion/confirmation |
| `omnigrex:plan-ready` | Confirmed planning completed; implementation is not authorized |
| `omnigrex:developing` | Developer work is active or pending |
| `omnigrex:reviewing` | Reviewer work is active or pending |
| `omnigrex:pr-ready` | The current Change Proposal head is approved and ready for human review |
| `omnigrex:waiting-for-input` | Work Item Issue is awaiting a human clarification response or exact-plan confirmation |
| `omnigrex:needs-human` | Autonomous progress stopped in a Human Handoff |

The state labels are observations maintained by Omnigrex, not commands to force a transition.
GitHub is the human-facing interface; PostgreSQL is the operational ledger.

## Start or reactivate work

Add `omnigrex:run` to the Work Item Issue or its **current** Omnigrex Change Proposal.
A Pull Request command requires an existing durable association to that Work Item in the same repository.
It cannot adopt an unrelated Pull Request, activate a historical or replaced Change Proposal, create a Work Item, or reopen a closed Issue.
A Workflow marker in a Pull Request alone is insufficient.

An accepted activation creates a new Workflow Attempt and resets retry and review budgets.
Retained Agent Sessions are reused when their configuration and state remain compatible.
Rejected Pull Request commands leave labels unchanged.

For [planning-only work](planning.md), use `omnigrex:plan` on an open, unstarted Issue with current write-or-higher repository authority.
`omnigrex:run` cannot convert planning into development, including after planning-ready; that transition remains follow-up #165.

## Review and retry budgets

Each Workflow Attempt permits at most three accepted Review Cycles.
A review counts only when the Reviewer App publishes it for the expected head and that head remains current.
A stale review does not consume the budget.
The third accepted changes-requesting review produces a Human Handoff.

Infrastructure failures allow one retry as a new Agent Turn in the same Agent Session.
They do not consume the review budget.
After a second failure, Omnigrex publishes diagnostics and hands control back to a human.
Default deployment settings allow two concurrent Agent Turns with a two-hour timeout per Turn.

Sometimes the GitHub mutation succeeded but a fresh verification read is unavailable.
The state label can remain developing or reviewing while Omnigrex verifies the result without another model prompt or duplicate mutation.
The default verification window is 30 minutes.
See [terminal verification](human-handoffs.md#when-publication-succeeded-but-verification-did-not) for what happens next.

## Changed Pull Request heads

Approval belongs to a particular head, not every later revision.
Head synchronization follows the current Workflow state and remaining review budget.
During a Human Handoff, pushes only update the tracked head; they do not resume automation.
After you explicitly reactivate work, Omnigrex verifies the current Pull Request identity, open status, head, and base before the first Agent Turn.

## Close and reopen an Issue

Closing the Issue fences new work, settles admitted mutations, and schedules retained Session data for deletion.
The default retention period is 30 days.
Reopening before collection cancels scheduled deletion but does not restart work.
For direct-development Work Items, add `omnigrex:run` after reopening when you want a new Workflow Attempt.
Planning Work Items cannot use that development transition; this increment does not introduce a completed/closed-planning reopening flow.
See [planning stopping behavior](planning.md#status-stopping-and-boundaries) for the supported boundary.

Once physical collection starts, deletion is irrevocable.
After collection, a new trigger creates new Agent Participants and Agent Sessions while the operational history remains in PostgreSQL.
Ask the operator about installation-specific [retention settings](../operators/retention.md).

Next: [Human handoffs](human-handoffs.md).

## Waiting for CI before review

By default, the orchestrator queues each new Reviewer Turn until relevant CI finishes.
The separate `omnigrex:waiting-for-ci` label appears alongside the review-stage label on both the Work Item Issue and current Change Proposal.
It is a scheduling observation, not a command or an approval state.
No Reviewer Agent Turn or Runtime Process is running for this wait.

One editable progress comment per location shows the reviewed head, CI state, deadline, and release reason.
Selected successful checks admit review; neutral and skipped checks also admit without being called successful.
Current selected failures return directly to the Developer without spending a Review Cycle.
Three automatic repairs are allowed between fully successful selected-CI admissions; the fourth needed repair produces Human Handoff.
Neutral/skipped, no CI, immediate bypass, and head changes do not reset this separate repair budget.
When no CI is observed after confirmed-empty discovery and registration grace, review can begin as no CI.
Incomplete evidence at the absolute deadline (two hours by default) instead produces Human Handoff.
Missing required checks do not become optional just because registration grace expires.

Repository owners can select [immediate concurrent review or an explicit expected-check set](../reference/repository-configuration.md#review-scheduling).
This does not change GitHub branch protection, review approval requirements, or local verification responsibilities.
The waiting label is removed when review is admitted or the wait is cancelled; review queued is distinct from actual Turn allocation.
New heads replace one pending target without extending its deadline.
CI completion never resumes a closed Workflow or existing Human Handoff without explicit human reactivation.

## Waiting for human input

`omnigrex:waiting-for-input` appears alongside the Stage label on the Work Item Issue only, not its Change Proposal.
It covers [clarification](clarification.md) from Planner, Developer or Reviewer and [exact-plan confirmation](planning.md), including a proposed pair with no open questions.
The latest published question round or proposed pair explains which response is needed; no separate progress comment is added.
This is an observation, not an activation command, implementation consent or Human Handoff.

An admitted response, including a partial answer, clears the indication when continuation queues, before the next Agent Turn starts.
A subsequent genuine request for input restores it.
Rejected commands, unrelated comments, edits, deletions and replayed earlier responses do not clear a newer wait.
Active execution, pending preparation or terminal-input revalidation, completed planning, PR-ready, Human Handoff and closed/closing or dormant Work Items are not human-input waits.

Updates reconcile durably and eventually after GitHub calls, retries and restarts; a label is not an atomic execution-state guarantee.
Upgrading an existing installation schedules label-only reconciliation for persisted collaboration records without asking humans to repeat answers or starting new Agent Turns.
This differs from the [CI wait](#waiting-for-ci-before-review), which requires no human response, and from [Human Handoff](human-handoffs.md), which stops autonomous progress for recovery.

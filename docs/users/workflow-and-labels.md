# Workflow and labels

[User guide](README.md) · [Documentation hub](../README.md)

## Read the labels

| Label | Meaning |
| --- | --- |
| `omnigrex:run` | Human command to create or reactivate a Workflow Attempt |
| `omnigrex:developing` | Developer work is active or pending |
| `omnigrex:reviewing` | Reviewer work is active or pending |
| `omnigrex:pr-ready` | The current Change Proposal head is approved and ready for human review |
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
Add `omnigrex:run` after reopening when you want a new Workflow Attempt.

Once physical collection starts, deletion is irrevocable.
After collection, a new trigger creates new Agent Participants and Agent Sessions while the operational history remains in PostgreSQL.
Ask the operator about installation-specific [retention settings](../operators/retention.md).

Next: [Human handoffs](human-handoffs.md).

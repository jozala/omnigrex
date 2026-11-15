# Plan an idea and explicitly authorize implementation

[User guide](README.md) · [Clarification and steering](clarification.md)

## Start and discuss

A collaborator with current repository **write, maintain or admin** permission adds `omnigrex:plan` to an open, unstarted Work Item Issue.
The Planner captures the exact original title, description and source; it never overwrites the Issue description.
Later description edits are context, not amendments to accepted decisions.
A suitable `PLANNER` Agent Profile under `.omnigrex/team/` is required only when invoking planning.
Missing or ambiguous configuration produces actionable Human Handoff feedback rather than substituting another Role.
Fix the Profile and use the existing [explicit recovery procedure](human-handoffs.md).
Direct `omnigrex:run` development still works with only Developer and Reviewer Profiles.

The built-in, versioned discovery procedure is supplied on every launch, including continuation of the retained Agent Session.
The Planner investigates code, tests and repository decisions before asking about facts it can discover itself.
It asks at most five consequential questions per round, recommends answers with evidence, and retains dependent questions for later rounds.
Use `/omnigrex answer R3`, `/omnigrex amend A7` and `/omnigrex steer` as described in the clarification guide.
Partial answers can resume discussion; silence and unanswered questions are not agreement.
Ordinary comments, reactions, quoted commands, and edits/deletions of accepted comments do not control planning.
Routine reversible details remain within the Planner's existing authority.

## Checkpoints and confirmation

At substantial milestones, revised directions, your request, and final review, the Planner publishes one readable Issue comment containing a **requirements brief** and a **technical plan**.
Each complete pair has one permanent short reference backed by an immutable internal revision; a new revision supersedes the earlier pair even when only one part changes.
Drafts distinguish confirmed decisions, assumptions/proposals and unresolved questions.
The evidence/history section shows repository references, the investigated commit, discovery version and bounded, escaped provenance references to the original request and selected accepted input.
Complete source text stays unchanged in durable collaboration history rather than being copied verbatim into every comment.
The view explicitly identifies shortened references or additional inputs; full records remain available through `get_collaboration_record` (`planning_request` / `planning_history`) and their original GitHub sources.
Earlier revisions and the full accepted discussion remain accessible through bounded collaboration retrieval.
Publication retries reconcile exact operation-marked artifacts rather than posting duplicates; oversized artifacts are rejected, never silently truncated.
If an accepted amendment or steering arrives after a Turn was prepared but before its checkpoint is reserved, publication is fenced without creating a revision.
The Planner yields so the orchestrator can deliver that input in the retained Session's next Turn, then reconsiders affected decisions before publishing a revised pair.
This ordinary discussion path does not require a human recovery command or reset retry/review budgets.

When material choices are settled, inspect the proposed pair and submit its actual short reference on the first nonblank line of a **new** Issue comment:

```text
/omnigrex confirm-plan P7
```

`P7` is illustrative, not a GitHub comment or question-round ID.
Only a write-or-higher collaborator can confirm the current published proposed revision.
The receipt acknowledges queueing, not successful completion or implementation authorization.
A final Planner Turn considers all pending input and current repository evidence.
Irrelevant repository changes may retain the revision with recorded revalidation; changed requirements or material commitments require a new published pair and renewed confirmation.
Structural checks alone do not establish semantic completeness or design correctness.

## Status, stopping and boundaries

- `omnigrex:planning`: active planning, normal question waiting or confirmation waiting.
- `omnigrex:plan-ready`: confirmed pair preserved after successful final validation.
- `omnigrex:needs-human`: actual failure/configuration/recovery problem, not ordinary discussion.

Normal waiting releases execution resources while preserving the Agent Session, Workflow Attempt and retry/review budgets.
There is no discussion deadline or inactivity-based acceptance.
Close the Issue to stop unwanted work using the existing [Workflow lifecycle](workflow-and-labels.md).
Use explicitly addressed steering/amendments to reopen discussion before implementation starts; label changes alone do not revise a plan.
If final label publication fails after planning has completed, resolve the operational problem and use the normal `omnigrex:run` Human Handoff recovery.
The orchestrator restores the already-completed `omnigrex:plan-ready` result and retries label reconciliation without a new Planner Turn, new confirmation, or changes to the immutable pair.
This is operational recovery, not reopening planning or authorizing implementation.

**Confirmation is shared understanding, not implementation authorization.**
Planning does not allocate Developer/Reviewer implementation work, publish code through MCP, create PRs or child Work Items, submit native reviews, or merge.
`omnigrex:run` cannot convert planning into development, including after planning-ready; use exact-pair implementation authorization below.
The Planner can perform bounded local investigations and experiments in the existing disposable Execution Environment with existing tool permissions.
This is the Developer/Reviewer trusted-repository baseline, not new shell/network confinement or provider-credential isolation.


## Conversation rhythm and confirmation

Answer a round with its permanent short target, for example `/omnigrex answer R3`, followed by natural agreement, qualifications or changes.
A round may briefly explain consequential implications of your last answer before asking what matters next.
The Planner can continue through several answer/question rounds without an initial full draft or a replacement plan after each answer.
Consolidated checkpoints are useful at substantial milestones, revised directions, your request, and final confirmation; there is no fixed number of questions between them.

Each checkpoint leads with a concise summary, material changes, unresolved decisions and next action.
Open the separate complete requirements brief and complete technical plan sections and review both before confirming with the actual published reference, for example `/omnigrex confirm-plan P2`.
The summary is not a substitute for the complete documents.
Evidence and history remain accessible separately, without repeating original requests and verbatim answers throughout the conversation.
Both complete documents remain one immutable pair in one comment; oversize content is rejected before publication so it can be rewritten concisely without losing commitments, never silently truncated or split.

Confirmation with a requested change is a change request, not approval of an unpublished modified plan.
A withdrawal or material objection invalidates earlier confirmation during active planning.
After reconciliation, confirm the exact pair again even if its documents remain unchanged; changed documents require a new pair and its own confirmation.
Answers and steering never substitute for exact-pair confirmation or implementation authorization, and steering/amendments before the first Developer admission reopen discussion without changing the earlier immutable pair.

## Authorize an exact pair

Review both complete documents, then submit a new Issue comment containing only:

```text
/omnigrex implement P7
```

Use the actual current proposed pair reference, not this illustrative `P7`.
This accepts that exact pair and requests implementation; a prior separate `confirm-plan` is not required.
It also works for an open eligible Work Item confirmed before this feature was installed, without repeating settled discussion or republishing unchanged documents.
Upgrading alone never authorizes development.
Repository write, maintain or admin permission is required and rechecked before first Developer admission.

No additional non-whitespace prose is allowed with the implementation command, including text on the same line or following lines.
For example, adding “but use PostgreSQL” rejects authorization rather than guessing whether consent is conditional.
Submit changes through `/omnigrex steer` or `/omnigrex amend A7` first, then authorize the resulting exact pair.
Answers, ordinary comments, reactions, Planner assertions, `confirm-plan` and `omnigrex:run` cannot substitute for this action.

A receipt means the request was recorded, not that validation passed or work started.
The Planner assesses all known input and current repository evidence before development is queued.
If the repository moved, the Planner checks whether those changes affect accepted behavior or material commitments; unrelated changes do not automatically invalidate the pair.
A current exact-pair authorization can survive immaterial input and repository movement without another approval click, but materially changed commitments require a revised complete pair and renewed authorization.
Authorization has no inactivity-based expiry; normal bounded infrastructure recovery still applies.

Concise milestone comments distinguish receipt/validation, queued development and started implementation.
Implementation starts when the first Developer Agent Turn is durably admitted, including while its Execution Environment is launching.
Queued preparation is not started work.
Duplicate commands, concurrent events and restarts cannot admit a second initial Developer Turn or reset budgets again.
After successful handoff, implementation has its normal infrastructure retry and review allowance, independent of planning's earlier retries.
Developer and Reviewer receive the exact accepted documents, original request, relevant decisions, constraints and evidence, not only a summary.
They may adjust reversible implementation suggestions, but material contradictions preventing safe implementation require Human Handoff rather than silently rewriting accepted intent.

## Withdraw unused implementation authorization

Before the first Developer admission, submit a new comment containing only:

```text
/omnigrex withdraw-implementation P7
```

This prevents unused authorization from starting development, without closing the Work Item or withdrawing pair confirmation.
This limited revocation also works during a preparation-failure Human Handoff before the first Developer admission; it leaves that Human Handoff and its budgets unchanged and schedules no work.
An earlier event or replay cannot restore it.
For commands with comparable immutable source order, a delayed older withdrawal likewise cannot revoke newer consent; webhook receipt time is not the human decision order.
A fresh explicit `implement` command may authorize the still-current valid pair again.
Steering/amendments accepted before admission also block development and return discussion to the Planner; assessment cannot silently resurrect withdrawn or invalidated authority.
Concurrent withdrawal, steering, closure and admission are serialized: feedback reflects which operation crossed the durable start boundary first.
After admission, limited withdrawal is rejected and does not claim to stop work.
Close the original Work Item Issue to use the existing stopping mechanism; removing state labels is not cancellation.

## Replanning after implementation starts

A Developer may return concrete evidence that an accepted material commitment is doubtful to the retained Planner.
Routine reversible implementation choices stay with the Developer.
Accepted human steering/amendments can also supply a material concern; ordinary comments do not.
Implementation and new Reviewer dispatch pause at a safe Turn boundary, after admitted operations and execution-stop/capture barriers settle.
Existing external CI need not be cancelled.
The Issue explains the concern, paused responsibility and governing revision; the same Work Item, Change Proposal and published history remain in use.
Safe useful unfinished work may be committed/published through existing tools without declaring review readiness; passing tests are not a prerequisite merely to preserve it.
Unpublished edits are not guaranteed to survive a fresh Execution Environment.

If the same pair and commitments remain valid, the Planner records a false-alarm rationale and resumes under still-valid authority, without another approval or duplicate documents.
A material change requires a linked **complete revised pair** explaining changed commitments.
For that current pair, `/omnigrex confirm-plan P8` confirms understanding only; `/omnigrex implement P8` accepts and authorizes validated resumption.
Limited `/omnigrex withdraw-implementation P8` can revoke unused resumption authority until the next Developer Turn is durably admitted, not stop already-admitted work.
Withdrawn or invalidated authority is never silently restored.
New input or repository/head movement before admission returns the same cycle to reassessment.

If authority was actually withdrawn, unchanged documents do not restore it: give fresh explicit authorization to the same still-valid pair before resumption.
No replacement pair is needed merely to express fresh consent.

Three actually dispatched autonomous returns are allowed per Workflow Attempt; a fourth requires Human Handoff.
False alarms count; duplicate requests, infrastructure retries and human discussion rounds do not.
An independently requested, authorized human direction change can be exempt, but replies inside an autonomous cycle never refund it or reset usage.
Resumption preserves retry/review/replanning usage and the original initial-admission history.
A material revised pair requires fresh native Reviewer evaluation even at the same code SHA, using remaining Review Cycles.
Relevant CI facts may be reused under their ordinary scope/provenance/freshness rules, but cannot waive new verification obligations.
Direct-development Work Items without an authorized plan and retained Planner keep their existing clarification/Human Handoff path.

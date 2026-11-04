# Explore an idea without starting implementation

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
Use `/omnigrex answer <round-id>`, `/omnigrex amend <response-id>` and `/omnigrex steer` as described in the clarification guide.
Partial answers can resume discussion; silence and unanswered questions are not agreement.
Ordinary comments, reactions, quoted commands, and edits/deletions of accepted comments do not control planning.
Routine reversible details remain within the Planner's existing authority.

## Checkpoints and confirmation

After meaningful changes, the Planner publishes one readable Issue comment containing a **requirements brief** and a **technical plan**.
Each complete pair has one immutable revision ID; a new revision supersedes the earlier pair even when only one part changes.
Drafts distinguish confirmed decisions, assumptions/proposals and unresolved questions.
The evidence/history section shows repository references, the investigated commit, discovery version and bounded, escaped provenance references to the original request and selected accepted input.
Complete source text stays unchanged in durable collaboration history rather than being copied verbatim into every comment.
The view explicitly identifies shortened references or additional inputs; full records remain available through `get_collaboration_record` (`planning_request` / `planning_history`) and their original GitHub sources.
Earlier revisions and the full accepted discussion remain accessible through bounded collaboration retrieval.
Publication retries reconcile exact operation-marked artifacts rather than posting duplicates; oversized artifacts are rejected, never silently truncated.

When material choices are settled, inspect the proposed pair and submit its actual revision ID on the first nonblank line of a **new** Issue comment:

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
This increment does not introduce dedicated defer, reject or completed-plan reopening commands.
If final label publication fails after planning has completed, resolve the operational problem and use the normal `omnigrex:run` Human Handoff recovery.
The orchestrator restores the already-completed `omnigrex:plan-ready` result and retries label reconciliation without a new Planner Turn, new confirmation, or changes to the immutable pair.
This is operational recovery, not reopening planning or authorizing implementation.

**Confirmation is shared understanding, not implementation authorization.**
Planning does not allocate Developer/Reviewer implementation work, publish code through MCP, create PRs or child Work Items, submit native reviews, or merge.
`omnigrex:run` cannot convert planning into development, including after planning-ready; that transition belongs to follow-up #165.
The Planner can perform bounded local investigations and experiments in the existing disposable Execution Environment with existing tool permissions.
This is the Developer/Reviewer trusted-repository baseline, not new shell/network confinement or provider-credential isolation.

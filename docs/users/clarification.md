# Answer questions and steer active work

[User guide](README.md) · [Human Handoffs](human-handoffs.md)

Developer and Reviewer can publish a clarification round on the Work Item Issue.
A round identifies individual questions and shows the exact round identifier to answer, for example `R7`.
Actual identifiers may be longer; copy them from the question comment rather than using a GitHub comment number.
Only one published round is current for a Work Item.

## Submit a complete or partial answer

Create a new Issue comment with one command on its first nonblank line, then your natural-language answer:

```text
/omnigrex answer R7
1. Preserve the existing API.
2. I cannot decide the retention period yet; what evidence would help?
```

Repository write, maintain, or admin permission is required.
Opening the Issue alone does not grant decision authority.
Answers may include numbered partial answers, disagreement, inability to decide, or “yes to all.”
Unanswered or ambiguous questions remain unresolved; acceptance of the command is not agreement with its interpretation.
Conflicting responses retain each author's provenance rather than applying last-answer-wins.
The agent must ask which decision governs before dependent action.

Omnigrex acknowledges acceptance and queueing with a response identifier, for example `A12`.
The acknowledgment is separate from the agent's interpretation and does not authorize implementation.
A rejected command receives feedback explaining missing permission, invalid syntax, an unknown target, a superseded round, or a lifecycle that cannot accept input.
If a round was superseded, use the current round identifier from the rejection or the new question comment.
A delayed answer to an old round cannot accept a revised recommendation.

## Amend accepted content

Editing or deleting an accepted comment does not change its accepted record.
Create an explicit amendment instead:

```text
/omnigrex amend A12
Correction to my earlier answer: retain the old API for one release.
```

The original response and author remain in history alongside the amendment and its actor.
An unambiguous correction may resolve a contradiction without another question round.
Materially changed questions or recommendations receive a new round identity; unchanged individual questions may retain their identity.

## Steer without a pending question

```text
/omnigrex steer
The deployment target has changed. Reconsider the compatibility assumptions before continuing.
```

Steering is accepted during active development/review or normal clarification waiting.
It is queued for the responsible agent's next safe Turn boundary, not an immediate stop command.
Use the existing closing/cancellation procedure when work must stop immediately.

Commands execute only on creation of a new comment on the Work Item Issue.
Ordinary comments, mentions, reactions, edits, deletions, quoted commands, fenced or indented code, and commands on later lines do not execute.
Do not combine two commands on the command line.
An ordinary comment is not implicit authorization or recovery.

## What happens while waiting

Expected clarification waiting is not a failure or Human Handoff.
The Runtime Process and disposable execution resources are released while the Agent Participant, Agent Session, Assignment Generation, Workflow Attempt, and retry/review budgets remain unchanged.
No `omnigrex:run` is needed and no timeout or inactivity period decides an unanswered question.
An accepted partial answer is enough to continue discussion.
All input pending at next-Turn preparation is delivered together, preserving its content, identity, author, and order, without an artificial collection delay.
Later input waits for another Turn; an active Turn is not automatically interrupted.
Pending input fences successful Stage handoff until delivery, but does not undo already-published commits or native reviews.
If a Stage transition committed first, the resulting current Stage determines routing, not an obsolete question's original Stage.

The agent must reconsider old questions and answers against relevant default-branch and Change Proposal changes.
An earlier answer is not approval of code that was never reviewed.
Closing/cancelling a Work Item fences continuation.
Answers, amendments, and steering cannot reactivate completed work, authorize implementation, or recover a Human Handoff.
For an actual `omnigrex:needs-human` condition, follow [explicit Human Handoff recovery](human-handoffs.md#reactivate-after-resolving-the-cause), which creates a fresh Workflow Attempt and resets its budgets.

### Long discussions and corrected decisions

An explicit amendment can correct an already-interpreted response, including an earlier round's still-relevant decision owned by the same Participant.
The earlier answer and interpretation remain in history; the agent must explicitly record its revised interpretation against the amendment.
Repeated amendments may target the same original response: a new correction makes the affected interpretation stale even when its evidence cited a previous correction.
Competing corrections remain available for disambiguation rather than silently selecting the last answer.
If current repository or Change Proposal evidence removes a question's premise, the agent may explicitly mark it obsolete with an explanation, retaining the earlier answer and interpretation without requiring a human to amend a formerly valid answer.
Obsolescence is not permanent: if current evidence restores the unchanged premise, the agent may explicitly renew the interpretation using delivered, read evidence and an explanation.
The obsolete entry has no governing human decision to amend; the renewed decision becomes effective on the same question identity, including in replacement rounds, while all earlier interpretations remain history.
Unchanged questions carried into a replacement round retain their effective accepted evidence; unanswered or amended-but-not-reinterpreted questions remain unresolved.
Materially revised questions use new identities and need new interpretation rather than inheriting approval.

Long discussions remain durable but are not copied in full into every prompt.
The agent receives a bounded projection and frozen collection counts, with `get_collaboration_record` available for lossless, ordered retrieval in bounded chunks.
All input pending at preparation remains available in that Turn without a batching delay; later input still belongs to the next Turn.
When pending contents are omitted from the envelope, successful handoff remains fenced until every record and chunk has been retrieved through the scoped capability.
Skipped, partial, or failed retrieval cannot acknowledge delivery; unread input survives an interrupted Turn and is included in the retained Session's continuation.

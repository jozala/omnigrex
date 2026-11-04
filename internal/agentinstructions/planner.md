### Omnigrex discovery procedure v1

This procedure is supplied by the current Omnigrex deployment on every launch, including Session Continuation.
Its version is `omnigrex-discovery/v1`; retain that version with each published checkpoint.
Earlier checkpoint procedure versions remain historical evidence, not current execution instructions.
Core discovery requires no repository-installed skill, runtime fetching, or delegation; no sub-agents are available.

#### Investigate before asking

Investigate repository facts with scoped collaboration reads, repository code, tests, documentation, and decisions before asking a human to look them up.
Capture the investigated commit and accessible evidence/source references.
Inspect differences between the captured original request and the current Issue description as context; edits do not amend accepted decisions.
Use bounded local investigations, tests, and disposable experiments within existing execution resource and Turn limits.
Do not deploy, mutate production data, incur additional paid-service costs, or publish code.
The existing trusted-repository execution baseline applies: whole-tool permissions are not shell-command, filesystem, network, or provider-credential security boundaries.

#### Work a dependent decision tree in rounds

Treat the idea as decisions with dependent branches, not a checklist to fill by guessing.
The frontier consists only of consequential questions whose prerequisites are settled.
Resolve discoverable facts yourself first; an uninvestigated fact is an unsettled prerequisite.
Ask at most five top-level consequential questions in a round, preferably fewer when substantial.
Do not hide independent decisions inside one question.
Retain the remaining frontier and downstream branches for later rounds, including when a partial answer triggers continuation.
Omitted answers and silence are unresolved, never agreement.

Number each question, give it a stable individual identity, explain the decision and material alternatives, and give an evidence-based recommendation and rationale.
Phrase recommendations clearly enough that the human can explicitly accept or reject them.
Do not ask dependent questions in the same round as their unresolved prerequisites.
Use request_clarification and the existing answer/amend/steer flow, rather than inventing another conversation mechanism.
Keep one current question set; explicitly replace materially changed questions and carry unresolved questions forward.
After replies, retrieve all required input, record explicit interpretations with response/source references, preserve competing responses, reconsider affected earlier assumptions, and recompute the frontier.
Do not silently replace an accepted decision with a later comment or your own preference.
Use explicit amendments and clarification where answers conflict or change earlier decisions.

Routine reversible implementation details remain within existing authority.
Consequential scope, security, data-retention, compatibility, external-dependency, and operational commitments require human discussion.
Distinguish confirmed human decisions, your proposals/assumptions, and unresolved questions throughout.
A recommendation is not a human decision.

#### Publish meaningful paired checkpoints

Publish a checkpoint after meaningful scope, decision, or approach changes, not after every answer or Turn.
Each immutable revision covers the complete requirements brief and technical plan as one pair, with explicit supersession of the earlier pair.
Draft checkpoints may be incomplete; expose missing decisions rather than inventing completeness.
Keep required behavior, material design commitments, and suggested local implementation details distinguishable.

The requirements brief records the problem, users and outcome, scope, non-goals, expected behavior, acceptance examples, constraints, confirmed human decisions, proposed assumptions, and unresolved questions.
The technical plan records relevant existing behavior/code/tests/decisions, material alternatives and rationale, component boundaries/contracts, compatibility/migration/failure concerns, delivery increments, and verification.
Retain the original request, accepted response provenance and exact contents, interpretations, evidence references, investigated revision, procedure version, and supersession/publication history through the shared collaboration boundary.
Never overwrite the human Issue description.

Lead the readable checkpoint with the idea/title, visible revision identity and status, what changed, requirements and approach in plain language, delivery and verification, and the next human action.
Put detailed repository references and decision provenance in an accessible evidence/history section, expandable where useful.
Do not silently truncate content to fit publication limits.

#### Confirm understanding, then revalidate

Use publish_plan_checkpoint for complete paired revisions and get_collaboration_record for planning_request/planning_history as needed.
After publishing a proposed pair, use request_clarification with empty questions to enter expected waiting for confirmation.
Use complete_planning only after a new current refresh_default_branch observation and final evidence-based revalidation.
When material choices are settled, publish the complete pair and ask an authorized human to submit `/omnigrex confirm-plan <revision-id>` for that exact current published revision.
Confirmation is shared understanding, not implementation authorization.
Answers, steering, comments, and inactivity do not confirm the pair or authorize implementation.
Wait without a discussion deadline or consuming retry/review budgets; normal waiting is not a Human Handoff.
A plan may leave routine local details open, but material uncertainty must be resolved or have an agreed verification step and response if its assumption fails.
Structured presence checks do not establish semantic completeness, reasoning quality, or design correctness.

On the final Planner Turn, deliver and consider pending input before completion and obtain current repository evidence.
Assess the relevance of repository changes rather than automatically invalidating or accepting a plan when a SHA changes.
If the confirmed revision remains applicable, retain it and record evidence-based revalidation.
If accepted requirements or material commitments change, publish a new paired revision and require renewed confirmation.
Unanswered consequential questions require clarification, not premature readiness.
Finish only in planning-ready after controller-validated confirmation and revalidation; do not allocate implementation, create PRs or child Work Items, publish code, merge, submit native reviews, or use planning confirmation as an implementation transition.
Closing the Issue uses existing stopping behavior; there are no new defer/reject/reopen commands.

#### Adaptation provenance and MIT notice

Adapted from Matt Pocock's `skills/productivity/grilling/SKILL.md` in `mattpocock/skills` at commit `4ae2317ddb22b046e26455e4f9c9b3aaaf62212d`.
Source Git blob: `d2f09a18e6a6c65c55f1271ba08dfeaf06f0bed3`; source SHA-256: `a146842ed93ca43d833550fa9adf02e58cb63c83fea9d4b4522995b82242d4e2`.
The pinned source and root license were supplied in Work Item #164 through the scoped Issue read boundary.
Adaptations bound rounds to five questions, replace sub-agent delegation with local investigation, retain routine-detail authority, and end at planning-only confirmation/revalidation.

MIT License

Copyright (c) 2026 Matt Pocock

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

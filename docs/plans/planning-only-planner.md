# Planning-only Planner implementation

Work Item: [#164](https://github.com/jozala/omnigrex/issues/164).
This is an implementation checklist, not evidence that the capability has shipped.

## Boundaries

Reuse the collaboration aggregate, immutable human input admission, safe-boundary scheduling, retained Agent Session, and operation-marked publication recovery from #163.
Add a planning entry and terminal planning-ready state to the code-defined Workflow, not a planning-to-development transition.
Planner MCP authority must exclude code publication, PR creation, native reviews, and implementation handoff.
Keep the existing trusted-repository execution baseline and avoid claims of shell/network or provider-credential confinement.

## Delivery

1. Supply a built-in versioned discovery procedure, adapted from the immutable upstream material supplied in the Issue, with preserved MIT notices.
2. Add Planner identity, Role Policy, Stage/outcome, optional Profile selection, and authorized planning activation without requiring a Planner Profile for direct development.
3. Extend the shared collaboration aggregate with original-request capture, immutable paired planning revisions, publication provenance and supersession, confirmation input, and final revalidation evidence.
4. Connect checkpoint publication and completion through the existing gateway/mutation/recovery contracts; fence final readiness on confirmation of the current published revision, delivered pending input, resolved consequential questions, and current repository evidence.
5. Document activation, discussion, checkpoint presentation, confirmation, stopping, labels, and the absence of implementation authorization.

## Implementation status

The draft now includes authorized planning activation; Stage-local optional Profile selection; planning Stage/outcome and labels; original-request capture; immutable paired checkpoints through shared collaboration, bounded retrieval and the Issue adapter; exact-revision confirmation; snapshot-backed final revalidation; pending-input fences; and planning-only completion.
The built-in versioned discovery procedure preserves pinned upstream MIT attribution on every launch and caps Planner question rounds at five.
The controlled Pi fixture covers retained rounds, a changed retention assumption, draft/proposed pairs, confirmation and final validation without implementation allocation/publication.
These tests establish deterministic orchestration, not reasoning quality or human approval.
The PR remains WIP pending final verification, exact-head CI/backend qualification, and the required real discovery transcript and actual human assessment.
Repositories using only the direct Developer/Reviewer path do not need a Planner Profile.

## Verification

Use behavior-focused tests for instruction composition, permissions, activation and direct-development compatibility, immutable artifact persistence/publication recovery, explicit confirmation authorization and targeting, pending-input fences, and planning-only completion.
Exercise several clarification rounds through controlled Pi Session continuation and recovery with an underspecified idea, repository-discoverable fact, consequential human choice, and changed earlier assumption.
Run focused tests, `mise run check`, `mise run test-integration`, and applicable controlled Workflow/recovery and backend qualification.
A controlled provider establishes orchestration behavior, not discovery quality.
Before acceptance, run a real Planner discovery scenario and obtain a real human assessment of its transcript and outputs against the Issue rubric.

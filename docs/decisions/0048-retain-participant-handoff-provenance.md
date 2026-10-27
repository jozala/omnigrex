---
status: accepted
date: 2026-10-27
decision-makers: [Omnigrex Developer]
---
# Retain Participant-local Human Handoff provenance

## Context
A Participant can remain `WAITING_FOR_HUMAN` while an explicitly activated Workflow Attempt selects only another Participant.
Requiring a Handoff on the immediately preceding Attempt incorrectly rejects later authorized Stage transitions back to that waiting Participant.

## Decision
Record the Workflow Attempt responsible for putting each Participant into Human Handoff.
A `CURRENT` preparation may reactivate that waiting Participant when its own Handoff belongs to the same Workflow and is inactive, and the preparation belongs to a later active, human-triggered Attempt.
The normal Workflow revision, Stage, preparation lease, active-Turn, Session Control Owner, immutable binding, recovery, and mutation-admission fences remain mandatory.
Clear the waiting provenance when the Participant is reactivated; a subsequent Handoff establishes new provenance.
Unused Participants remain waiting rather than being eagerly reactivated with the selected Participant.

For existing waiting Participants, the forward migration reconstructs provenance from the most recent completed Handoff in their Workflow that postdates their creation, excluding deleted Participants and Participants with a recorded later reactivation.
Absent matching evidence, provenance remains null and continuation fails closed.
This schema upgrade does not activate any Workflow or create any Agent Turn.

## Rationale
Merely searching for any historical Handoff at preparation time would not tie authorization to the selected Participant.
Eagerly marking every waiting Participant active would blur Participant lifecycle and Session-control boundaries.
Participant-local provenance survives unrelated successful Attempts while preserving explicit human activation as the authority to continue autonomous work.
No Session is replaced, rebound, or given a new history to bypass the check.

## Links
- [Work Item 142](https://github.com/jozala/omnigrex/issues/142)
- [Orchestrator-owned Session control](../adr/0003-orchestrator-owns-session-control.md)
- [Stage Assignments and Agent Participants](../adr/0008-separate-stage-assignments-from-agent-participants.md)
- [Activation from the current Change Proposal](0021-activate-from-current-change-proposal.md)

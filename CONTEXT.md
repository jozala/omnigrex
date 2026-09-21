# Omnigrex

Omnigrex coordinates software-development work performed by specialized agents under deterministic workflow control.
This language separates durable work coordination from the runtime-specific mechanics used to execute an agent.

## Work

**Work Item**:
A unit of software-development work supervised by a human and represented by a durable collaboration artifact.
_Avoid_: Task, ticket

**Workflow**:
The complete coordination process associated with one Work Item.
_Avoid_: Pipeline, job

**Workflow Definition**:
The immutable code-defined Stage graph, Role bindings, accepted purposes, outcomes, and review limits used by every Workflow in one deployment.
_Avoid_: Persisted workflow template, user-authored workflow

**Workflow Attempt**:
A bounded period of autonomous work started or resumed by a human, with its own retry and review budgets.
_Avoid_: Run, execution

**Review Cycle**:
One Reviewer evaluation of the current Change Proposal within a Workflow Attempt.
_Avoid_: Review iteration, feedback loop

**Stage**:
A stable named position in a Workflow Definition that selects one Role and defines accepted purposes and outcome transitions.
_Avoid_: Status, step number

**Change Proposal**:
The reviewable set of repository changes produced for a Work Item.
_Avoid_: Patch, branch

**Blocking Finding**:
A review finding that must be resolved before a Change Proposal is ready for human review.
_Avoid_: Error, rejection

**Non-blocking Finding**:
A review finding that can remain unresolved when a Change Proposal is handed to a human.
_Avoid_: Nit, suggestion

## Agents

**Role**:
A named set of responsibilities, goals, permissions, and decision boundaries within a Workflow.
_Avoid_: Job title, prompt

**Agent Profile**:
A repository-owned named, versioned set of instructions and configuration that declares the Role performed by an agent.
_Avoid_: Persona, agent definition

**Assignment Generation**:
A durable epoch that groups the Agent Participants and Stage Assignments selected for one period of Workflow responsibility.
_Avoid_: Attempt, session generation

**Agent Participant**:
The durable identity of one Agent Profile participating in a Work Item during one Assignment Generation, including its immutable Runtime Profile binding.
_Avoid_: Role, Agent Session, worker

**Stage Assignment**:
The immutable binding from one Stage in an Assignment Generation to an Agent Participant whose Agent Profile belongs to the Stage's Role.
_Avoid_: Mutable assignment status, Agent Participant

**Agent Session**:
The persistent conversational context owned by one Agent Participant and reusable across multiple Agent Turns and Stage Assignments that select that Participant.
_Avoid_: Chat, run

**Agent Turn**:
One prompt-response interaction within an Agent Session, including any capability use performed before the response completes.
_Avoid_: Agent Run, invocation

**Runtime Profile**:
An immutable, versioned contract describing how an Agent Session is executed and restored.
_Avoid_: Agent type, container configuration

**Runtime Process**:
A disposable execution of a Runtime Profile that hosts one or more active Agent Turns without owning the durable Agent Session.
_Avoid_: Agent, session

**Session Continuation**:
The ability to continue an existing Agent Session with its prior context.
_Avoid_: Resume, reload

**History Replay**:
The ability to present previously emitted Agent Session history to a client.
_Avoid_: Session Continuation, transcript restoration

**Agent Event**:
A runtime-neutral observation emitted while an Agent Session is created, continued, or used.
_Avoid_: Log line, transcript entry

## Control

**Control Owner**:
The single actor authorized to submit the next prompt to an Agent Session.
_Avoid_: Session mode, operator

**Role Policy**:
The immutable deployment policy that maps a Role to capabilities, credential authorities, runtime hardening, workspace isolation, and human-control eligibility.
_Avoid_: Agent Profile, Workflow transition

**Human Handoff**:
A Workflow state in which autonomous progress stops and responsibility is explicitly returned to a human.
_Avoid_: Failure, cancellation

**Durable Artifact**:
A human-readable outcome retained with the project so future work does not depend on an Agent Session.
_Avoid_: Agent memory, transcript

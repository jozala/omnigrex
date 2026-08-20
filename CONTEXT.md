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

**Workflow Attempt**:
A bounded period of autonomous work started or resumed by a human, with its own retry and review budgets.
_Avoid_: Run, execution

**Review Cycle**:
One Reviewer evaluation of the current Change Proposal within a Workflow Attempt.
_Avoid_: Review iteration, feedback loop

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
A named, versioned set of instructions and configuration used by an agent performing a Role.
_Avoid_: Persona, agent definition

**Agent Assignment**:
The binding of a Work Item, Role, Agent Profile identity, and immutable Runtime Profile for a period of responsibility.
_Avoid_: Agent job, delegation

**Agent Session**:
The persistent conversational context owned by one Agent Assignment and reusable across multiple Agent Turns.
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

**Human Handoff**:
A Workflow state in which autonomous progress stops and responsibility is explicitly returned to a human.
_Avoid_: Failure, cancellation

**Durable Artifact**:
A human-readable outcome retained with the project so future work does not depend on an Agent Session.
_Avoid_: Agent memory, transcript

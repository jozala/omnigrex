# Resolve a Human Handoff

[User guide](README.md) · [Documentation hub](../README.md)

For ordinary agent questions, use [clarification](clarification.md), which preserves the current Workflow Attempt without `omnigrex:run`.
A Human Handoff means autonomous progress has stopped and a person must resolve the reported condition.
Start with the `omnigrex:needs-human` explanation on the Issue rather than repeatedly adding the command label.

## Read the explanation

The main comment leads with the stopping condition, proposed human intervention, and supported continuation.
When available, agent-reported explanation, observations, completed work and material uncertainty are labeled separately.
Required actions and risks stay visible; supporting evidence and technical diagnostics are expandable.
Reported diagnoses are not independent proof that other failures cannot exist.
The Orchestrator supplies the Work Item/current Change Proposal association and tracked head.
When the tracked head has moved, the source observation head remains separate: earlier test claims do not qualify the newer revision.

Older persisted handoffs and infrastructure/budget failures may have no contextual report.
Their comments use the known stopping condition and troubleshooting guides without inventing an owner, completed work or root cause.
A bounded legacy diagnostic excerpt is explicitly labeled when the entire diagnostic cannot be published.
Operational safety reports do not establish that a Workflow is stopped or eligible for reactivation; follow the linked operator procedure instead.

## Find the responsible person

| Reported condition | Who can usually resolve it |
| --- | --- |
| Ambiguous acceptance criteria or an unresolved product choice | Work Item author or project maintainer |
| Review or CI-repair budget exhausted, incomplete CI evidence at deadline, or a Change Proposal requiring human judgment | Project maintainer |
| Invalid Agent Profile, missing skill, or unsupported repository environment | [Repository owner](../repository-owners/troubleshooting.md) |
| Provider access, GitHub App permissions, missing images, storage, or runtime failure | [Operator](../operators/troubleshooting.md) |

## Reactivate after resolving the cause

1. Read the diagnostic and correct the reported prerequisite or ambiguity.
2. If repository configuration changed, publish it to the default branch and ensure required skills are also present in the Change Proposal checkout.
3. Ask the operator to rerun preflight when installation prerequisites or repository configuration were involved.
4. Confirm that the Issue is open.
5. Add `omnigrex:run` again to the Issue or current Change Proposal.

This creates a fresh Workflow Attempt and resets retry, review, and CI-repair budgets without erasing history.
Successful CI, comments and pushes alone do not reactivate a handed-off Workflow.
Do not edit state labels to simulate progress.

## When publication succeeded but verification did not

Omnigrex distinguishes a successful GitHub mutation from acceptance of its Workflow outcome.
If review submission or a Developer handoff succeeded but verification could not complete, the diagnostic explains that distinction.
During the verification window, Omnigrex retries reads without prompting the agent or repeating the mutation.

After the resulting Human Handoff, an explicit `omnigrex:run` can first revalidate the recorded intent when the Stage, Role, Assignment Generation, and automation-controlled Session still match.
Otherwise normal reactivation applies.
A remembered conversation or final model response alone is not proof of a successful terminal intent.
Let Omnigrex reconcile its own recorded operations rather than manually recreating reviews or publications.

## What to include when asking for help

Share the Issue and Pull Request, relevant head SHA, diagnostic category, and what changed since the handoff.
An operator can correlate safe identifiers with deployment logs.
Keep provider keys, private keys, raw prompts, and private repository content out of public reports.

Next: [Repository troubleshooting](../repository-owners/troubleshooting.md) or [operator troubleshooting](../operators/troubleshooting.md).

## CI admission handoffs

A waiting deadline means required evidence is missing, pending, or unavailable, not that the code failed.
Check the selected contexts, head, deadline, and permission diagnostics in the progress comment; repair CI registration or access before reactivation.
A repair-budget handoff means three automatic returns to the Developer have occurred since the last fully successful selected-CI admission.
Inspect the reported failures and scoped diagnostics before deciding whether to repair manually or explicitly start another Attempt.
Late CI completion alone cannot resume either handoff.

# Resolve a Human Handoff

[User guide](README.md) · [Documentation hub](../README.md)

For ordinary agent questions, use [clarification](clarification.md), which preserves the current Workflow Attempt without `omnigrex:run`.
A Human Handoff means autonomous progress has stopped and a person must resolve the reported condition.
Start with the `omnigrex:needs-human` explanation on the Issue rather than repeatedly adding the command label.

## Find the responsible person

| Reported condition | Who can usually resolve it |
| --- | --- |
| Ambiguous acceptance criteria or an unresolved product choice | Work Item author or project maintainer |
| Review budget exhausted or a Change Proposal requiring human judgment | Project maintainer |
| Invalid Agent Profile, missing skill, or unsupported repository environment | [Repository owner](../repository-owners/troubleshooting.md) |
| Provider access, GitHub App permissions, missing images, storage, or runtime failure | [Operator](../operators/troubleshooting.md) |

## Reactivate after resolving the cause

1. Read the diagnostic and correct the reported prerequisite or ambiguity.
2. If repository configuration changed, publish it to the default branch and ensure required skills are also present in the Change Proposal checkout.
3. Ask the operator to rerun preflight when installation prerequisites or repository configuration were involved.
4. Confirm that the Issue is open.
5. Add `omnigrex:run` again to the Issue or current Change Proposal.

This creates a fresh Workflow Attempt and resets retry and review budgets without erasing history.
Comments and pushes alone do not reactivate a handed-off Workflow.
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

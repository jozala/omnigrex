# Your first Work Item

[User guide](README.md) · [Documentation hub](../README.md)

## Before you start

Ask your repository owner to confirm that both GitHub Apps have access, the required Agent Profiles are on the default branch, and operator preflight and webhook verification have passed.
The repository must contain the code, instructions, and tools needed to build and verify the requested change.
Choose a small change with an observable result for your first Work Item.

## 1. Describe the work in a GitHub Issue

Explain the problem, the desired behavior, relevant context, and acceptance criteria.
For example:

```markdown
## Problem
The setup instructions omit the required configuration file.

## Desired result
Update the setup guide so a new contributor can find the example configuration.

## Acceptance criteria
- Link to the existing example file from the setup instructions.
- Explain where the contributor should copy it.
- Keep secret values out of the documentation.
```

The current Workflow starts with implementation, not a Product Owner requirements interview.
Resolve important product ambiguities before triggering work.

## 2. Add the command label

Add `omnigrex:run` to the Issue once.
Omnigrex consumes the command and publishes state labels as work progresses.
If the label is missing, ask the repository owner to investigate [repository onboarding](../repository-owners/connect-a-repository.md#confirm-readiness).

## 3. Follow development and review

Look for `omnigrex:developing`, the linked Pull Request, and `omnigrex:reviewing`.
Developer handoff comments identify the exact head submitted for review.
Reviewer findings may send the Change Proposal back to the Developer within the bounded review budget.

Agent-authored comments and reviews end with a signature such as:

```text
_By Omnigrex: `profile-name` [Role Display Name]_
```

The profile name identifies the participating Agent Profile even when several Profiles share a GitHub App identity.
Historical comments and system Human Handoff diagnostics may not have this signature.

## 4. Review the outcome

When `omnigrex:pr-ready` appears, inspect the current Pull Request head, tests, and review evidence before deciding whether to merge.
The label means the agent review accepted that head; it does not establish that required CI checks passed.
Use GitHub branch protection to enforce mandatory checks.

If `omnigrex:needs-human` appears, read the explanation and follow [Human handoffs](human-handoffs.md).
An ordinary comment or a push during a Human Handoff does not restart automation.

You have completed the first journey when you can identify the Change Proposal and its review result, or understand the blocker and who must resolve it.

Next: [Workflow and labels](workflow-and-labels.md).

# Use Omnigrex through GitHub

Use this guide when your repository is already connected and you want agents to work on an Issue.
You interact through GitHub Issues, Pull Requests, labels, and agent comments.
You do not need deployment access or provider credentials.

## Start here

1. [Your first Work Item](first-work-item.md): prepare an Issue, start work, and review the result.
2. [Workflow and labels](workflow-and-labels.md): understand state, review budgets, and closing or reopening work.
3. [Human handoffs](human-handoffs.md): resolve blockers and explicitly reactivate automation.

If the repository is not ready, ask its owner to follow [Connect a repository](../repository-owners/connect-a-repository.md).

## What you supervise

An Issue represents a **Work Item**.
Omnigrex coordinates its **Workflow** through Developer and Reviewer agents and publishes a **Change Proposal** as a Pull Request.
The agents have separate persistent Agent Sessions and separate GitHub App identities for development and native reviews.
You remain responsible for assessing the result and deciding whether to merge it.

[Documentation hub](../README.md)

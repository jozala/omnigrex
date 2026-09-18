---
status: accepted
---

# Use separate Developer and Reviewer GitHub Apps

The first iteration uses one Developer/Orchestrator GitHub App for webhooks, repository changes, Pull Requests, labels, comments, and read operations, and a second Reviewer App only for native review submission and reconciliation.
GitHub does not allow the author identity of a Pull Request to approve its own Pull Request, so one App could not represent an independent Reviewer with native approval state.

## Consequences

Both Apps must be installed on every managed repository and configured with separate private keys and least-privilege permissions.
Only the Developer/Orchestrator App sends webhooks, which avoids duplicate event streams while preserving distinct GitHub identities.
Credential selection follows the operation rather than the performing Role, as specified in [ADR 0009](./0009-select-github-app-credentials-by-operation.md).

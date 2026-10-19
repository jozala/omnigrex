# Role-based documentation cleanup

## Goal

Provide GitHub-first Markdown documentation for everyday users, repository owners, installation operators, and Omnigrex contributors.
Rewrite the entry points and fill onboarding gaps while preserving authoritative operational procedures and accepted specifications.
The organization follows [decision 0030](../decisions/0030-organize-documentation-around-reader-responsibilities.md).

## Content map

| Existing material | Authoritative destination |
| --- | --- |
| README introduction and documentation list | Root README and documentation hub |
| Start a Workflow, labels, retry/review budgets, close/reopen behavior | User guides |
| Agent Profiles, repository skills, environment files, tool paths | Repository-owner guides and repository configuration reference |
| Operator instruction directory and path allowlist | Operator agent policy |
| Supported deployment, prerequisites, release distribution, ingress, deployment | Operator requirements and installation |
| App creation and permissions, private keys, secrets and rotation | Operator GitHub App and secrets guides |
| Backend contracts, native provisioning, storage constraints | Operator backend guide |
| Doctor, readiness, recovery diagnostics | Operator health and troubleshooting guides |
| Tracing and application metrics configuration | Operator observability guide |
| Backup, supplemental exports, restore, upgrades and retention | Operator maintenance guides |
| Source builds, protocol diagnostics, provider smoke, Workflow/recovery/upgrade qualification | Contributor development, testing and runtime qualification |
| Omnigrex-specific development image | Contributor development-image guide |
| Historical plans, experiments and migration evidence | Supporting indexes with explicit historical context |

## Delivery and verification

1. Create the hub, audience indexes, current-behavior guides, and focused reference pages.
2. Replace the README with a concise product introduction and audience routes.
3. Replace the old operator guide with an anchor-preserving relocation index and update incoming links.
4. Update agent instructions and environment examples to point to authoritative destinations.
5. Keep the executable restore and Compose-selection tests attached to the new procedure pages.
6. Add a dependency-free local Markdown link and anchor check to the normal verification gate.
7. Run focused documentation-example tests and `mise run check`.

Read each journey from the root README through its success condition.
Do not interpret historical qualification or accepted decisions as evidence of a currently deployed release.
Do not modify `first-iteration.md` or change runtime behavior as part of this cleanup.

# Configure GitHub Apps

[Operator guide](README.md) · [Installation](installation.md)

Create two GitHub Apps under the account or organization owning the managed repositories.
Names are operator-selected, but numeric App IDs and private keys must differ.
User authorization, callback URLs, and OAuth features are unnecessary.
Separate identities allow the Reviewer to approve a Pull Request authored by the Developer App.

## Developer App

Configure these repository permissions:

| Permission | Access |
| --- | --- |
| Metadata | Read-only |
| Checks | Read-only |
| Actions | Read-only |
| Commit statuses | Read-only |
| Administration | Read-only |
| Contents | Read and write |
| Issues | Read and write |
| Pull requests | Read and write |
| Workflows | Read and write |

Subscribe to exactly **Issues**, **Issue comment**, **Pull request**, **Pull request review**, and **Repository** events.
After upgrading to human clarification support, add **Issue comment** to the Developer App subscriptions and rerun preflight.
The Repository subscription supplies `repository.created` for onboarding.
GitHub also supplies the default `installation.created` and `installation_repositories.added` deliveries.

Enable the webhook with:

| Setting | Value |
| --- | --- |
| URL | `https://PUBLIC_HOST/webhooks/github` |
| Content type | `application/json` |
| Secret | Exact contents of `secrets/github-webhook-secret` |
| SSL verification | Enabled |

Use public HTTPS and exactly `/webhooks/github`, with no URL credentials, query, or fragment.

## Reviewer App

| Permission | Access |
| --- | --- |
| Metadata | Read-only |
| Contents | Read-only |
| Pull requests | Read and write |

Select no webhook events and disable its webhook.
Do not reuse the Developer identity or key.
CI diagnostic reads use the Developer App, not expanded Reviewer permissions.
After an upgrade adding required-CI evidence, add Commit statuses and Administration read permissions to the Developer App, approve the updated installation permissions, and rerun preflight before starting Agent Turns.
Administration read is needed for complete classic branch-protection reads; no administration write, Checks write, or CI-completion webhook subscription is required.
App configuration and installation tokens must match the exact permission tables, so updating only the App or only its installation is insufficient.
Doctor rejects missing permissions and broader permissions such as Administration write.
Required-policy permission failures remain explicit unavailable/partial evidence, never an empty requirement set.
See [CI evidence](../reference/ci-evidence.md) for the read contract and recovery guidance.

## Keys and installation

Generate one RSA private key per App in GitHub and store PKCS#1 or PKCS#8 PEM files at:

```text
secrets/github-developer-private-key.pem
secrets/github-reviewer-private-key.pem
```

Set their numeric App IDs in the private deployment `.env`:

```dotenv
OMNIGREX_GITHUB_DEVELOPER_APP_ID=12345
OMNIGREX_GITHUB_REVIEWER_APP_ID=67890
```

Install both Apps on the owning account or organization and grant each access to every managed repository.
For selected-repository installations, add new repositories to both Apps before preflight.
Coordinate access approvals with repository owners and organization administrators.
Omnigrex creates short-lived installation tokens; no personal access token is required.

Follow [secret placement and groups](secrets.md) before starting the controller.
Verify an actual Developer App delivery receives HTTP 202 as described in [installation](installation.md#7-verify-and-hand-over).

See [ADR 0005](../adr/0005-use-separate-developer-and-reviewer-github-apps.md) and [ADR 0009](../adr/0009-select-github-app-credentials-by-operation.md) for identity and credential selection.

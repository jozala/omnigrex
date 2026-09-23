# Dogfooding Record

This document records durable, non-secret evidence from first-iteration live exercises.
It separates completed rehearsal evidence from the final requirement to dogfood Omnigrex on its own repository.

## Disposable Repository Rehearsal

Date: 2026-09-07.

Repository: [`jozala/omnigrex-testrun`](https://github.com/jozala/omnigrex-testrun).

Work Item: [Issue #9, Create simple Hello World application in Go](https://github.com/jozala/omnigrex-testrun/issues/9).

Change Proposal: [Pull Request #10](https://github.com/jozala/omnigrex-testrun/pull/10).

The Developer App `omnigrex-developer[bot]`, GitHub actor ID `325664305`, opened the Pull Request at 2026-09-07 07:06:12 UTC.
The Pull Request marker records Workflow `068b0d0e-e82c-4ad8-864e-f78961151175` and Developer Assignment `6d206c22-6571-4ba4-a464-a5009a9a1912`.
The resulting head was `410320ebf9f27db7116d74dd6fd46dd23255aff1` against base `4ae5e25b8f2069fb2015726b26c76ea61496e3da`.

The Reviewer App `omnigrex-reviewer[bot]`, GitHub actor ID `325665116`, approved that exact head at 2026-09-07 07:07:18 UTC.
The review marker records Reviewer Assignment `3d74032e-d397-4718-a71d-8d99cf8a191b`.
The native review is [pull request review 5128988573](https://github.com/jozala/omnigrex-testrun/pull/10#pullrequestreview-5128988573).

Issue #9 and Pull Request #10 both reached `omnigrex:pr-ready`.
The Change Proposal contained one App-authored commit, three expected files, and no CI configuration.

After deployment of the preflight implementation and disabled-Reviewer-webhook correction through commit `fc73c17`, the operator ran:

```sh
docker compose exec orchestrator \
  /usr/local/bin/omnigrex doctor --repository jozala/omnigrex-testrun
```

All configuration, secret-format, GitHub App, PostgreSQL, Docker, Runtime Profile, Agent Profile, ACP, and MCP checks reported `PASS`.

The architecture and automated tests prove that Runtime Processes do not receive GitHub App private keys, App JWTs, installation tokens, `GH_TOKEN`, or `GITHUB_TOKEN`.
The live rehearsal did not retain a separate process-environment capture, network trace, provider credential, prompt transcript, or secret.

## Rehearsal Limits

The disposable-repository rehearsal proves the basic Developer-to-Reviewer approval path and human handoff.
It does not by itself complete the Phase 10 requirement to dogfood Omnigrex on this repository.

The rehearsal did not include:

- A requested-change cycle followed by Developer Session Continuation.
- An orchestrator restart during an active Agent Turn.
- A backup and restore drill followed by Session Continuation.
- Runtime Process IDs proving fresh containers for the same Agent Sessions.
- Auditable network evidence excluding direct unauthenticated GitHub API requests.
- The exact deployed orchestrator revision and Runtime Profile image digest used during the 07:05 UTC Workflow.

The live Reviewer API gate separately exercises both approved and changes-requested native review submissions, but it does not coordinate a complete Workflow.

## Omnigrex Repository Gate

The final Phase 10 dogfood must run against [`jozala/omnigrex`](https://github.com/jozala/omnigrex) after the Agent Profiles in `.omnigrex/team/` are merged into its default branch.

Use this acceptance sequence:

1. Deploy the intended Omnigrex revision and record its Git commit and exact Runtime Profile image digest.
2. Run `doctor --repository jozala/omnigrex` and retain sanitized all-pass output.
3. Create a deliberately small Issue with explicit acceptance criteria.
4. Add `omnigrex:run` once.
5. Verify that the Developer App creates a normal Pull Request through the Workflow.
6. Request one small correction and verify that the same Developer Assignment and Agent Session continue in a fresh Runtime Process.
7. Restart the orchestrator during a controlled active Agent Turn and verify recovery through the same Agent Session.
8. Approve the current head through the Reviewer App and verify that both Issue and Pull Request converge to `omnigrex:pr-ready`.
9. Verify that Developer and Reviewer use distinct Assignments, Agent Sessions, and GitHub App identities.
10. Verify from Runtime Process configuration and auditable network policy that neither Role receives GitHub credentials or accesses the GitHub API directly.
11. Record only non-secret identifiers, timestamps, head SHAs, review URLs, process IDs, and outcomes in this document.

Do not record provider credentials, private keys, webhook secrets, App JWTs, installation tokens, prompt text, or raw tool output.

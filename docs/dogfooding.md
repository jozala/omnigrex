# Dogfooding Record

This document records durable, non-secret evidence from first-iteration live exercises.
It separates completed rehearsal evidence from the final requirement to dogfood Omnigrex on its own repository.

## Disposable Repository Rehearsal

Date: 2026-09-07.

This section records the historical OpenCode exercise; the later Pi/Microsandbox exercise is recorded separately below.

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

### Rehearsal Limits

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

## Pi on Microsandbox with Compose-backed Integration Tests

Date: 2026-10-15.

Repository: [`jozala/omnigrex-testrun`](https://github.com/jozala/omnigrex-testrun).

Work Item: [Issue #19, Add PostgreSQL-backed greeting counter with Docker Compose integration tests](https://github.com/jozala/omnigrex-testrun/issues/19).

Change Proposal: [Pull Request #20](https://github.com/jozala/omnigrex-testrun/pull/20).

The operator confirmed successful replacement of the previous installation with Pi on Linux/amd64 Microsandbox and Docker Compose execution inside the microVM.
The selected published release and exact public image references are recorded in [Step 17 release evidence](./plans/2026-10-13-pi-only-final-qualification.md#published-release-and-operator-cutover).
This record does not include an independent deployed-container inventory.

### Initial publication and approval

The Developer App opened the Change Proposal at 2026-10-15 21:47:57 UTC.
Its public marker identifies Workflow `f94df33f-f0dd-41f7-963f-24a93e7b91b7` and Developer Participant `3da30cdc-b077-4b19-91dd-6e193552d3b4`.
The [Developer handoff](https://github.com/jozala/omnigrex-testrun/pull/20#issuecomment-6202976672) records head `59f0418b8424bc5611ecb0a5a76728a3f57defdd`.
It reports passing unit tests, nine PostgreSQL integration tests, ordinary and integration-tagged `go vet`, and `./scripts/test-integration.sh` with Compose startup and cleanup.
These are Developer-reported verification results, not independently rerun commands by the documentation author.

The separate Reviewer App [approved that exact head](https://github.com/jozala/omnigrex-testrun/pull/20#pullrequestreview-5540398652) at 2026-10-15 21:53:36 UTC.
Its public marker identifies Reviewer Participant `30c81e84-47ba-4da7-b523-8afcbd48c950` and Profile `janusz`.
Both the Work Item and Change Proposal carried `omnigrex:pr-ready` when the initial approval outcome was checked.

### Human-requested correction cycle

The human [requested changes](https://github.com/jozala/omnigrex-testrun/pull/20#pullrequestreview-5540547148) at 2026-10-15 22:10:58 UTC for the documented Go minimum and integration-test database isolation.
The same Reviewer Participant [superseded its approval with requested changes](https://github.com/jozala/omnigrex-testrun/pull/20#pullrequestreview-5540610673) at 22:18:45 UTC after reporting an independent reproduction of the destructive database-selection defect.
The [revised Developer handoff](https://github.com/jozala/omnigrex-testrun/pull/20#issuecomment-6203501605) at 22:22:52 UTC identifies the same Developer Participant and new head `405a7966c85aaa6d1c1df37484cf7a011940f83a`.
It reports fixes for both findings and passing Compose-backed tests, including preservation of a seeded counter and cleanup of isolated schema-test databases.
At the recorded follow-up check, the Change Proposal remained open with `omnigrex:reviewing`; the initial approval does not approve this revised head.

### Evidence boundaries and remaining dogfooding

This exercise establishes live publication, distinct Developer/Reviewer GitHub App identities and Participants, an initial approval outcome, guest Compose use confirmed by the operator, and a human-requested revision cycle.
Public markers show Participant continuity but do not expose exact Agent Session identities or prove that later Turns ran in fresh Execution Environments.
The exercise does not establish a live orchestrator-restart recovery, History Replay, Testcontainers execution, Omnigrex's own integration suite, or production backup/restore.
The operator chose to skip the historical-release reset/recovery rehearsal and old-state backup; no successful published-release rehearsal is claimed.
The broader gate below remains separate from this disposable-repository evidence.

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

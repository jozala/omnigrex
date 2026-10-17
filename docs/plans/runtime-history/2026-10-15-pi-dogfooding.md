# Pi Runtime Delivery Dogfooding (2026-10-15)

This document records durable, non-secret evidence from live Pi exercises.
It separates completed disposable-repository exercises from the final requirement to dogfood Omnigrex on its own repository.

## Pi on Microsandbox with Compose-backed Integration Tests

Date: 2026-10-15.

Repository: [`jozala/omnigrex-testrun`](https://github.com/jozala/omnigrex-testrun).

Work Item: [Issue #19, Add PostgreSQL-backed greeting counter with Docker Compose integration tests](https://github.com/jozala/omnigrex-testrun/issues/19).

Change Proposal: [Pull Request #20](https://github.com/jozala/omnigrex-testrun/pull/20).

The operator confirmed successful Pi installation on Linux/amd64 Microsandbox and Docker Compose execution inside the microVM.
The selected published release and exact public image references are recorded in [Step 17 release evidence](../2026-10-13-pi-only-final-qualification.md#published-release-and-operator-cutover).
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

# Dogfooding Record

This document records durable, non-secret live evidence for the supported release baseline.
The [clean baseline decision](./decisions/0024-establish-a-clean-supported-release-baseline.md) requires `pi-rpc/v2` and a fresh installation.

## Current Baseline Validation

Source qualification is recorded in the [implementation plan](./plans/2026-10-16-clean-runtime-baseline.md).
Published-image deployment and a live Work Item for this baseline must be recorded after publication and operator deployment.
Record the exact release commit, image digests, Work Item and Change Proposal links, approved head, and observed outcomes.
Distinguish operator-confirmed execution from commands independently rerun by the evidence author.

## Omnigrex Repository Gate

The live dogfood must run against [`jozala/omnigrex`](https://github.com/jozala/omnigrex) after the supported Agent Profiles in `.omnigrex/team/` are merged into its default branch.

Use this acceptance sequence:

1. Deploy the intended revision and record its Git commit and exact Runtime Profile image digest.
2. Run `doctor --repository jozala/omnigrex` and retain sanitized all-pass output.
3. Create a deliberately small Work Item with explicit acceptance criteria and add `omnigrex:run` once.
4. Verify that the Developer App publishes a normal Change Proposal through the Workflow.
5. Request one small correction and verify that the same Developer Participant and Agent Session continue in a fresh Execution Environment.
6. Restart the orchestrator during a controlled active Agent Turn and verify recovery through the same Agent Session.
7. Verify Reviewer App approval of the current head and convergence of the Work Item and Change Proposal to `omnigrex:pr-ready`.
8. Verify distinct Developer and Reviewer Participants, Agent Sessions, and GitHub App identities.
9. Verify from execution configuration and auditable network policy that neither Role receives GitHub credentials or accesses GitHub directly.
10. Record only non-secret identifiers, timestamps, head SHAs, review URLs, execution references, and outcomes here.

Do not record provider credentials, private keys, webhook secrets, App JWTs, installation tokens, prompt text, or raw tool output.

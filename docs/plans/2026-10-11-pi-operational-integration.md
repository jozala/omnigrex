# Pi operational integration and backend CI

## Published-release status

The release boundary described below was passed by [main CI run 38643789132](https://github.com/jozala/omnigrex/actions/runs/38643789132) for commit `3125265f30f5cbc97096babde01cbd8dae7388e5` on 2026-10-14.
Docker qualification, Linux/KVM qualification, publication, and both published-image deployment smokes succeeded.
The [Step 17 release record](2026-10-13-pi-only-final-qualification.md#published-release-and-operator-cutover) contains the exact public image references and subsequent operator evidence.
The implementation narrative below describes the historical Step 15 milestone; Step 17 removed its transitional OpenCode paths.

## Prerequisite and scope

This implements the source and qualification work for Step 15 of the [combined plan](2026-10-05-pi-and-pluggable-execution-backends.md), following [Pi upgrades and state recovery](2026-10-10-pi-upgrades-and-state-recovery.md).
The [release-gating decision](../decisions/0018-gate-pi-publication-on-both-backend-deployments.md) records the preparatory runtime selection and deployment/CI policy.
The operator installation guide owns the authoritative [Pi deployment procedure](../operators/installation.md).

## Operational integration

Startup and doctor construct the same exact Runtime Profile registry from configured image/platform pairs.
Pi-only deployments no longer require an unused OpenCode image, while existing OpenCode deployments remain operational during preparation.
When both are configured, the shared storage probe uses Pi and runtime diagnostics cover both contracts.
Repository Agent Profiles continue selecting their explicit runtime, and retained Participant bindings remain immutable.

Startup qualifies every configured golden-resource inventory before serving webhooks or starting Workflow workers.
Readiness includes selected-backend checks, configured and protected exact-image availability, retained-state availability, PostgreSQL, and every configured target's pending compatibility evidence.
Docker writable-path probes derive their mounts and temporary paths from the immutable Runtime Profile.
The Microsandbox backend retains its native SDK/KVM preflight and rejects startup without usable device access.
Repository environment checks validate each Role's selected runtime and requested capabilities rather than assuming the deployment's former OpenCode contract.

Doctor validates effective Pi permissions, current instructions, and selected-provider API-key shape through the production rendering adapter.
The separate effective-runtime check verifies requested Pi models and thinking values without fallback.
Its owned-runner probes create random exact Session identities in disposable temporary `/state`, use fixture-only provider credentials and authenticated diagnostic MCP discovery, and submit no model prompt.
Successful return requires bounded process stop and removal, with cleanup failures included in the reported error.
Pi-specific configuration stays in composition and the adapter; the existing dependency checks remain intact.

## Deployment and publication artifacts

`deploy/compose.pi.yaml` contains the shared PostgreSQL, Pi image validator, control plane, file-backed secrets, and persistent storage.
Exactly one backend overlay supplies management authority: Docker socket access or native KVM access.
Neither deployment has a build context, and the static Docker image requires no Microsandbox libraries or KVM device.
The native overlay provides a protected home/cache/journal volume and disk-backed image-import scratch outside retained Session state.

Image-only smoke stages deployment files and disposable fixture secrets into a separate directory with unique resource names.
It isolates Compose from ambient operator configuration, verifies liveness/readiness and independent operational doctor checks, checks that no retained runtime-state files exist after diagnostics, and removes only its own resources.
The native smoke also removes device access and verifies that normal startup fails with the explicit KVM prerequisite error.
Fixture GitHub App identities intentionally fail external installation and repository Profile checks; these smokes do not qualify live GitHub credentials or mutate a live repository.

CI now requires Docker qualification and an ephemeral Linux/amd64 KVM job before main-branch publication.
Both execute shared conformance, Pi protocol/configuration/Session lifecycle, complete Workflows, protected-state recovery, failure recovery, upgrades, and image-only smoke.
Native qualification additionally exercises guest Docker, Compose, and Testcontainers.
The KVM job fails rather than skipping tests when the device is absent or unusable.

The publication path adds the pinned Pi 1.0.4 / Node 24.14.1 golden image and SDK-enabled native orchestrator alongside the preparatory existing images.
It checks platform and non-root ownership, records resolved digests, and feeds those exact digests to post-publication smoke jobs for both backends.
Deployment files remain commit-pinned raw artifacts, following the existing distribution mechanism.

## Verification evidence

Observed passing local checks and Docker gates:

```sh
mise install
mise run check
mise run test-integration
mise run test-pi-integration
mise run test-pi-workflow
mise run test-pi-recovery
mise run test-pi-upgrade
mise run test-pi-deployment
OMNIGREX_PI_WORKFLOW_TEST_PATTERN='^TestPiProtectedStateRecovery$' mise run test-pi-workflow
```

Observed passing Linux/amd64 KVM gates, with the approved remote Docker connection and host-specific KVM group supplied:

```sh
mise run test-microsandbox-integration
mise run test-pi-microsandbox-integration
mise run test-pi-microsandbox-workflow
mise run test-pi-microsandbox-recovery
mise run test-pi-microsandbox-upgrade
OMNIGREX_PI_WORKFLOW_TEST_PATTERN='^TestPiProtectedStateRecovery$' mise run test-pi-microsandbox-workflow
```

The daemon-host smoke script also passed for Docker/Linux amd64 and Microsandbox/Linux amd64:

```sh
OMNIGREX_PI_BACKEND=docker sh scripts/test-pi-deployment.sh
OMNIGREX_PI_BACKEND=microsandbox sh scripts/test-pi-deployment.sh
```

Docker/Linux arm64 qualification ran through Docker Desktop, and both image-only Linux deployments ran on the approved KVM host without a host Go or mise installation.
Compose rendering coverage verifies backend-specific authority, absence of source builds, and that the opposite backend's group setting is unnecessary.
Actionlint 1.7.7 validated the updated workflow.
The existing live GitHub and destructive stable-stack Compose gates remain opt-in; the new smokes use separate disposable resources.

Qualification found and fixed native image import overflowing the control-plane tmpfs, missing disposable Session identity in the production runner probe, and executable diagnostic storage contradicting the restrictive diagnostic contract.
A cold amd64 build also exposed an unavailable checksum-locked Alpine `nghttp2-libs` package; both architecture locks now select the available 1.70.0 package with verified checksums, and the cold image build passed.
An initial broad integration run failed the existing 80 ms human-prompt lease timing assertion under load; its focused rerun and the final complete integration gate passed without changing that test or its assertions.
One concurrent Docker Desktop deployment smoke reported an unhealthy control plane without preserving its startup logs; subsequent serial and concurrent reruns passed, and failed smokes now retain the control-plane log output before removing their fixtures.
The earlier unhealthy result has no established failure cause and is not presented as a diagnosed resource-exhaustion incident.
Connection values, fixture digests, private keys, temporary files, and deployment metadata are excluded from this record.

## Release boundary at implementation time

Source implementation and both-backend host qualification are complete.
At this milestone, the new GitHub-hosted KVM job, GHCR publication, package visibility, and post-publication exact-digest smoke still required merged-release evidence.
The source-native smoke's imported fixture alias is test evidence only and cannot substitute for a published exact registry image.
The retained OpenCode implementation, legacy image hint, image publication, and old deployment bundle were transitional until Step 17 removed them.
Step 16 supplied the operator procedures and source-fixture rehearsal, including reconciliation of the original Work Items.

## Independent review corrections

Round 1 used separate Standards and Spec review subagents over the complete tracked and untracked Step 15 working tree against its pre-implementation HEAD.
The Standards reviewer reported no actionable findings.
The Spec reviewer reproduced that Docker archive import synthesizes a different Microsandbox manifest, making the published-image smoke's original registry-digest alias invalid under production verification.

Published inputs now use native `msb image pull` into the smoke's fresh isolated cache, matching the operator deployment procedure and preserving the original manifest/index and platform linkage.
Archive import and imported-digest aliases remain confined to source fixtures.
A script-level regression failed before the correction and now verifies exact registry provisioning, rejection of archive import in the published path, and abort before startup on provisioning failure.
A separate native integration test uses a fresh SDK process and cache, confirms that the actual registry-pinned image is initially unavailable, pulls it through the operator CLI, and passes production immutable-image/platform verification afterward.

After the correction, `mise run check`, `mise run test-integration`, the focused script regression, and the Linux/KVM fresh-cache registry-pull test passed.
Round 2 used two fresh independent Standards and Spec subagents to review every tracked change and untracked addition, including the correction and regression tests.
Both reported no remaining or new actionable findings, and the Spec reviewer explicitly confirmed that the published-image digest issue is resolved in the implementation.
Their independent verification included uncached provisioning-regression and Compose/backend tests; the Spec reviewer additionally reran Docker image-only startup, readiness, disposable Pi/MCP diagnostics, and retained-state checks successfully.
These clean source-review results do not replace the merged-release publication and exact published-Pi smoke gates described above.

### Readiness deadline follow-up

A subsequent operator review reproduced that the old fixed 20-second healthcheck HTTP timeout cut off the Pi bundle's configured 60-second readiness budget despite its 70-second Compose deadline.
Startup and the standalone healthcheck now share readiness-duration parsing and validation, with a five-second client response allowance and overflow rejection.
Pi defaults are 60 seconds for the handler, 65 seconds for the HTTP client, and 70 seconds for Compose; the legacy bundles preserve their 15/20/25-second defaults.
All bundles expose `OMNIGREX_HEALTHCHECK_TIMEOUT` so operators can raise the outer deadline alongside a larger readiness budget, following the operator guide.

The production-handler regression failed after approximately 20 seconds before the correction and now succeeds after a 21-second readiness check within a 60-second budget.
Additional coverage verifies an expired readiness handler returning failure, a stalled HTTP peer bounded by the client deadline, invalid and overflowing budgets rejected before an HTTP request, and rendered default/custom Compose deadlines exceeding the HTTP deadline.
The focused race-enabled regressions, `mise run check`, `mise run test-integration`, Docker image-only smoke, and Linux/KVM Microsandbox image-only smoke passed after the correction.
Fresh Standards and Spec subagents subsequently reviewed the entire updated tracked and untracked Step 15 diff, explicitly confirmed the healthcheck finding resolved, and each reported no remaining or new actionable findings.
Their independent uncached verification included the 21-second production-readiness regression, bounded stalls, default/custom Compose budgets, and Docker image-only deployment smoke.

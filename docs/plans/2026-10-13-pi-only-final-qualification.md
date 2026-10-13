# Pi-only source and final backend qualification

## Scope and delivery boundary

This delivers the source implementation and backend qualification for Step 17 of the [combined plan](2026-10-05-pi-and-pluggable-execution-backends.md), following [operator documentation and cutover rehearsal](2026-10-11-pi-operator-documentation-and-cutover.md).
Pi is the only supported coding-agent runtime in the application, repository Profiles, images, deployment defaults, and CI publication path.
Docker and Microsandbox remain separately supported Execution Backends.
The [operator guide](../operator-guide.md#one-time-opencode-to-pi-cutover) remains authoritative for release selection, clean disposal, coordinated recovery, and production cutover.

Source qualification is complete; the full Step 17 completion gate is still open.
No release was committed, pushed, or published during this implementation.
Exact published-image smoke and historical-source clean-cutover rehearsal therefore remain pending.
The earlier [live Microsandbox Work Item](2026-10-11-pi-operator-documentation-and-cutover.md#live-candidate-workflow-validation-between-steps-16-and-17) is candidate evidence, not live dogfooding of the final Pi-only release.
Final-release isolated dogfooding and operator-controlled production cutover remain pending.

## Pi-only implementation

- Removed the OpenCode adapter, ACP client/protocol, OpenCode image and deployment bundle, old configuration fields, Role hardening type, doctor probe, build/publication tasks, and obsolete runtime-specific tests.
- Removed the legacy bootstrap-image route and ACP-shaped prompt/content interfaces from Runtime Process launch and Agent Turn coordination.
- Kept the neutral execution, storage, workspace, Session, and agent boundaries, with Pi-specific mechanics owned by the Pi adapter.
- Required one exact Pi image/platform binding and explicit `runtime: pi-rpc/v1` in repository Profiles; `steps`, `variant`, legacy runtimes, and legacy qualification artifacts are rejected.
- Retained backend-scoped version-2 Pi upgrade artifacts, exact retained Participant images, protected captures, and both backend qualification suites.
- Converted runtime-neutral launch, control, mutation, retention, and recovery fixtures to Pi rather than deleting their behavioral coverage.
- Replaced the old stable-resource Compose gate with disposable Pi full-stack deployment, persistence, webhook, isolation, and readiness checks.
- Preserved immutable Pi tool-storage paths whose historical spelling includes `opencode`; those paths do not install, configure, or execute OpenCode.

The application rejects unsupported runtime history before backend recovery, webhook serving, or Worker admission, including collected Participant/Session history and orphaned stored contracts.
This enforces the clean-cutover requirement even when no protected OpenCode state remains.
Ordinary Pi image changes continue to select independent fresh Participants without rebinding retained ones.

## Durable prompt evidence and recovery

Agent Turn settlement consumes neutral `agent.Outcome` observations directly.
Only positively admitted completion or abort observations become terminal responses; rejected, failed, unknown, and invalid observations remain classified failures.
A known runtime limit prevents successful terminal acceptance, while transport loss or a deadline still permits corroborating a previously successful terminal mutation.
No prompt outcome substitutes for the stop/capture/removal barrier.

Migration `000036_neutral_prompt_outcomes.sql` adds neutral prompt classifications without rewriting historical migrations or converting conversations.
Credential-free checkpoint JSON records admission, status, and an optional limit flag, never arbitrary failure metadata or response text.
Unsupported historical checkpoint representations lead to a fenced prerequisite Human Handoff instead of permanently leasing the verification job.
Neutral coordinator regressions retain replay-before-prompt admission, fence loss during replay, and uncertain release after completed prompting.
The [Pi-only decision](../decisions/0020-finish-pi-only-runtime-and-neutral-outcome-evidence.md) records this final transition.

## Deployment and cleanup

The root development stack and image-only bundles select Pi, and publication produces Pi plus the Docker and native Microsandbox control planes.
CI still requires both backend gates and repeats deployment smoke against the published exact digests.
Removing OpenCode source does not remove the operator's historical reset rehearsal.
That rehearsal now requires an externally selected old-release Compose bundle and both exact published source images; there is no current-source OpenCode build fallback.

Destructive cleanup is authorized only for an exact generated or validated isolated Compose file set.
Rejecting an unsafe historical bundle cannot authorize teardown of that bundle.
Registry and PostgreSQL fixture cleanup removes anonymous volumes through the exact owned container, without global pruning.
The final host inventories matched their baseline after qualification, with no fixture containers, volumes, networks, or VM wrappers left behind.

## Qualification findings and corrections

The first integration runs found migrated fixture errors in Profile runtime identity, PostgreSQL UUID parameter typing, and capture-key setup.
They also found that the Go race fixture installed mise data under Pi's bounded `/tmp` instead of allocated disk-backed storage.
Corrections preserve the invalid-input, human-control, capture ownership, synchronized Go execution, and intentional-race assertions.

A response-file polling race produced an archive `unexpected EOF` after capture completed.
The fixture now publishes that response marker atomically, preserving the original mid-request quiescence assertions.
Independent Docker inspection reproduced an OOM kill in the 64 MiB golden-resource probe, with a 128 MiB positive control succeeding.
The production probe now has bounded 256 MiB headroom with swap disabled for pinned Node/Pi CLI startup; its image/resource checks remain intact.

Final PostgreSQL integration exposed overlapping same-kind Workflow GitHub-effect leases caused by a statement snapshot predating acquisition of the Workflow lock.
Claiming now rechecks eligibility using a fresh READ COMMITTED statement after acquiring the locks and continues searching unrelated Workflows when the selected Workflow became blocked.
A second coordinated regression covers an uncommitted heartbeat crossing its old visible expiry.
Every same-kind `LEASED` row blocks admission until reclamation or acknowledgement completes its locked state transition, so a skipped locked renewal cannot be bypassed.
The tests cover both effect kinds, unrelated-Workflow progress, independent-kind admission, heartbeat commit/cancellation, stale-fence rejection, and eventual successor admission.

## Observed verification

Local checks passed with the pinned toolchain:

```sh
mise run check
mise exec -- go test -race ./internal/runtime/session ./internal/agentturn -count=1
mise exec -- go test -tags=integration ./internal/store ./internal/doctor ./internal/deployment ./internal/runtime/docker -run '^$'
```

Docker Desktop was unavailable, so Docker-backed verification ran on the authorized Linux/amd64 KVM host using isolated source staging, checksum-pinned mise 2026.8.10, and Go 1.27.0.
Final aggregate execution used `umask 022` and uncached host Go tests.

Passed Docker qualification:

```sh
mise run test-docker-integration
```

This aggregate passed the complete integration suite, shared Docker conformance, Pi protocol/configuration/Session tests, controlled Developer-to-Reviewer Workflow, protected-state recovery, slow working-revision bootstrap, all twelve recovery scenarios, Pi 1.0.3-to-1.0.4 upgrade/restore, and disposable image-only full-stack deployment.

Passed Microsandbox qualification with the host KVM group supplied:

```sh
mise run test-microsandbox
mise run test-microsandbox-integration
OMNIGREX_PI_WORKFLOW_TEST_PATTERN='^(TestPiControlledWorkflow|TestPiProtectedStateRecovery|TestPiSlowRepositoryBootstrap)$' mise run test-pi-microsandbox-workflow
mise run test-pi-microsandbox-recovery
```

The complete native gate passed shared conformance, protocol, Workflow, recovery, upgrades, and deployment, including missing-KVM startup rejection.
Subsequent changes invalidated only the affected Docker integration and native nested-container/Workflow/recovery evidence; those checks were repeated successfully after the corrections.
The native Agent Turn environment ran Docker, Compose, Testcontainers, and the agreed Docker-backed development tests without the host Docker socket.

The final lease regressions also passed five repetitions:

```sh
mise exec -- go test -race -tags=integration -timeout 20m -count=5 ./internal/store -run '^TestWorkflowGitHubEffectClaims'
```

That run completed 500 contention rounds and twenty coordinated heartbeat/expiry cases, and the regressions passed again inside the final integration suite.
Independent read-only core and deployment/backend reviews confirmed their original findings resolved and reported no remaining actionable P1/P2 findings.
Source checksums were unchanged across the final remote execution, and the original host services and storage inventories were preserved.

Fixture GitHub installation failures in deployment doctor output are expected; independent operational checks pass without granting external mutation authority.
These source-fixture results do not qualify production credentials, public package pulls, a published release, or a final live dogfooding Work Item.
Complete the remaining release, historical-source rehearsal, isolated dogfooding, and operator-controlled cutover gates using the operator guide before closing Step 17 or its original Work Items.

## Subsequent external review corrections

External review found that generated historical volume names still retained custom `driver_opts` or storage drivers, allowing a bind-backed or network-backed volume to reference existing installation data.
The validator now permits only default or explicit `local` drivers with an empty backing-options object across every historical volume.
Twelve regressions use real Compose rendering for bind, NFS, and custom-driver definitions across all four storage resources and verify that the backing configuration survives the generated-name override.
Those cases failed before the correction and now reject the bundle before startup or destructive cleanup authorization.
A real-rendered local-volume positive control prevents unrelated validation failures from masking the regression.
The fixture directory uses its physical path so Compose's canonical secret paths also match on macOS.

The same review reproduced that the contention test incorrectly required the higher-priority A job to win despite `SKIP LOCKED` permitting the other eligible job.
Both claim tests now check exclusivity, unrelated-Workflow progress, membership in the seeded eligible jobs, and admission of the remaining A job after acknowledgement independently of winner order.
Half of the stress rounds deliberately lock the higher-priority candidate to require a lower-priority win and verify that the skipped candidate becomes claimable afterward.
The production claim policy is unchanged by this test correction.

Local `mise run check`, the race-enabled cutover regressions, shell syntax, and diff whitespace checks passed.
The authorized Linux/KVM host passed the two corrected claim tests with `-count=20`, covering 2,000 contention rounds and 1,000 forced skips.
The exact `mise run test-integration` task and both `test-pi-deployment` and `test-pi-microsandbox-deployment` smokes also passed after the corrections.
An independent read-only review subagent confirmed both findings resolved and reported no new actionable P1/P2 findings in this correction.
The host's original services, container identities, volumes, and networks remained unchanged, with no fixture resources left behind.

## Integration with current main

The PR conflict resolution merges `main` through `558bee2` while retaining Pi-only execution, neutral outcomes, and sealed capture ownership.
Current Change Proposal activation, tracking-only head synchronization, authoritative reactivation verification, and operation-scoped GitHub credentials now run through those boundaries.
Each independent GitHub observation and trusted publication inspection acquires a current credential, including retries after slow repository preparation or transient failures.
The upstream APK refresh is retained in the renamed orchestrator package lock.
The independently numbered upstream decisions are canonical records 0021-0023, with their original filename links retained as pointers.

The new scoped CI tools remain available through the owned Pi MCP extension.
An integration review found that its generic tool-error handling discarded the upstream CI error classifications.
The extension now reconstructs allowlisted CI classification messages for the five CI tools while keeping transport errors, unknown payloads, and arbitrary error reasons generic.
All 351 actual HTTP/SDK boundary tests pass, including malformed responses, malicious reasons, transport impersonation, and endpoint redirects.

Local `mise run check` and integration-tag compilation passed after resolution.
The authorized qualification host passed `mise run test-integration` and every remaining Docker aggregate constituent in order, reusing the already successful shared integration result.
The complete `mise run test-microsandbox` aggregate also passed, including both-backend CI boundary tests, controlled Workflows, all twelve recovery scenarios, upgrade/restore, nested Docker/Compose/Testcontainers, and image-only deployment.
A stale integration assertion was updated to require the separate credential acquisition for sealed publication inspection without changing its settlement or successor assertions.
An independent final review confirmed the CI error-boundary finding resolved and reported no other actionable P1/P2 integration findings against either merge parent.
The original host services and storage inventories remained unchanged after qualification.

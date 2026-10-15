# Pi operator documentation and cutover rehearsal

## Current delivery status

The [Step 17 release record](2026-10-13-pi-only-final-qualification.md#published-release-and-operator-cutover) supersedes the historical pending-publication status below.
The merged Pi-only release passed both-backend CI, image publication, and both exact published-image deployment smokes.
The operator subsequently confirmed a fresh Pi/Microsandbox installation and a [live Compose-backed Work Item](../dogfooding.md#pi-on-microsandbox-with-compose-backed-integration-tests).
The historical-release rehearsal was attempted but stopped at source-bundle isolation validation before stack startup; the operator chose to skip it and did not report a completed production backup/restore drill.
The source-fixture rehearsal evidence in this document remains valid for its recorded scope, but the full published-release reset/recovery gate is not claimed complete.

## Prerequisite and delivery boundary

This delivers the documentation and source-fixture rehearsal work for Step 16 of the [combined plan](2026-10-05-pi-and-pluggable-execution-backends.md), following the [Step 15 operational integration](2026-10-11-pi-operational-integration.md).
The [operator guide](../operator-guide.md) is authoritative for current operational procedures; the historical reset and rehearsal procedures are in the [OpenCode-to-Pi migration guide](../migrations/opencode-to-pi.md).
Existing decisions [0005](../decisions/0005-separate-execution-backends-from-coordination.md), [0006](../decisions/0006-use-pi-through-an-agent-specific-adapter.md), [0007](../decisions/0007-supply-provider-api-keys-through-disposable-files.md), [0009](../decisions/0009-pin-participant-images-and-use-working-revision-tools.md), [0011](../decisions/0011-bind-retained-state-and-validate-offline-session-archives.md), [0017](../decisions/0017-bind-pi-qualification-to-backend-contracts.md), and [0018](../decisions/0018-gate-pi-publication-on-both-backend-deployments.md) supply the policy; this step adds no new runtime or migration policy.

Source-host qualification is not published-release evidence.
At implementation time the latest successful main CI run, [38095244804](https://github.com/jozala/omnigrex/actions/runs/38095244804), published the older Docker/OpenCode pair and had no KVM or published-Pi smoke job.
The full Step 16 completion gate then awaited Step 15's merged release publication, public package availability, and both-backend exact published-image rehearsal.
Step 17 owned OpenCode removal, Pi-only final qualification, isolated Microsandbox dogfooding, and the operator-controlled production cutover.

## Documentation changes

The README links deployment, cutover, and recovery to the operator guide and exposes the isolated rehearsal entry points.
The guide documents one selected Backend, capability differences, KVM/native prerequisites, trust and resource limits, and explicit rejection without fallback.
It makes Pi credentials and Profile permissions authoritative, including optional thinking, denied omitted tools, Reviewer edit/write restrictions, explicitly authorized checkout-local skills, and disabled uncontrolled resource discovery.
It retains labeled OpenCode procedures only for the preparatory path.

The Pi golden image is Debian-based, so the previous Alpine repository-image example is unsuitable.
`agent/environments/pi-example/Dockerfile` demonstrates external construction with an exact Pi golden image, an approved exact Debian package version, and restored non-root identity.
The guide distinguishes executable/resource qualification from ancestry, repository build/test qualification from doctor checks, and new independent Participants from copied-state Pi upgrade qualification.

Backup documentation now includes the PostgreSQL ownership/contract ledger, retained Session data, protected Workspace Captures, and the selected native backend's private dispatch journal.
It excludes provider/MCP authority, VM disks and snapshots, scratch, and image caches from ordinary Session recovery archives.
Restore preserves backend, namespace, platform, Participant/Session and exact image bindings, requires original-host fencing, and re-provisions protected registry images on a compatible replacement host.
Supplemental Session archives remain distinct from coordinated recovery sets.

Cutover documentation explicitly disposes old operational and runtime data after a verified stopped-state backup, replaces Profiles and credentials, starts a fresh installation, and accounts for surviving GitHub artifacts and deliveries.
Rollback cannot merge OpenCode and Pi ledgers or reverse GitHub mutations after fresh Pi work.
Later Pi qualification stays directional and copy-based; retained Participants never silently rebind.
`CONTEXT.md` and the existing decision records link and clarify these established invariants.

## Executable rehearsal

`test-pi-cutover` and `test-pi-microsandbox-cutover` extend the existing isolated image-only smoke through a fixed sourced rehearsal helper.
They do not select ambient deployment secrets or stable installation resources.
They start the actual OpenCode control-plane bundle with fixture credentials, seed unmistakable old ledger/storage markers, validate a pre-reset PostgreSQL dump and runtime-state archive, explicitly remove all four old volumes, and refuse Pi startup if old storage survives.
The target is the existing image-only Pi bundle plus exactly one backend overlay.

Pi startup/readiness and independent doctor checks exercise the production runner and authenticated diagnostic MCP path with disposable Session storage and no model prompt.
External GitHub and repository Profile checks fail intentionally for fixture App identities, while all independent operational checks must pass.
The recovery phase uses PostgreSQL custom-format dump/restore and tar archives to restore state/capture markers and UID/GID 10001 with private file modes after container recreation.
The native phase includes and restores only the journal, preserves the separately provisioned image cache, checks empty native inventory before copying, and repeats the existing missing-KVM startup rejection.

The disposal, Session, capture, and journal marker files are synthetic operational fixtures, not converted conversations or authentic sealed capture/dispatch records.
Actual fenced Pi execution, protected Participant state recovery, continuation after fresh environments, and provider-free History Replay are separately exercised by the existing Workflow and upgrade gates on both backends.
Those complementary checks do not claim a populated production recovery set or live GitHub deployment was restored in this session.

Four failure regressions run the script with a controlled Docker boundary: failed backup blocks procedural reset, surviving legacy storage blocks Pi startup, and unavailable process or volume inventories do not establish stopped execution or absent storage.
They additionally check that Compose operations retain the generated isolated project despite an ambient production project setting.

## Observed verification

Passed local checks and Docker/Linux arm64 source-fixture qualification:

```sh
mise install
mise run check
mise exec -- go test -race ./internal/deployment -run 'TestPiCutover' -count=1
mise run test-integration
mise run test-pi-cutover
OMNIGREX_PI_WORKFLOW_TEST_PATTERN='^(TestPiControlledWorkflow|TestPiProtectedStateRecovery)$' mise run test-pi-workflow
mise run test-pi-upgrade
```

Passed native Linux/amd64 qualification on the approved KVM host, with its authorized Docker connection and KVM group supplied:

```sh
OMNIGREX_PI_CUTOVER_REHEARSAL=1 OMNIGREX_PI_BACKEND=microsandbox sh scripts/test-pi-deployment.sh
OMNIGREX_PI_WORKFLOW_TEST_PATTERN='^(TestPiControlledWorkflow|TestPiProtectedStateRecovery)$' mise run test-pi-microsandbox-workflow
mise run test-pi-microsandbox-upgrade
```

The native rehearsal runs on the daemon host with staged source; the Workflow and upgrade gates support the authorized remote Docker connection.
The source-host rehearsal needs no host Go or mise installation.
The Debian repository image example was built with an approved exact jq package version and checked for the added tool, Pi/Node resources, and UID/GID 10001.
Fixture credentials, addresses, namespaces, temporary paths, and image digests are excluded from this record.

An initial source rehearsal attempted to launch a mutable OpenCode fixture tag and correctly failed exact-registry validation; source rehearsal images now resolve through a disposable registry.
The first native recovery attempt found that a diagnostic-only installation has no Turn dispatch journal yet; the journal round-trip now seeds an explicitly synthetic non-record marker rather than claiming diagnostic probes create production dispatch records.
One Docker doctor resource probe exited with status 137 during qualification; a full repeat passed without changing limits or checks, and no OOM cause was established.
An interrupted short SSH tool deadline and a source transfer containing macOS metadata were environmental rehearsal failures; a clean metadata-free transfer and sufficient execution deadline produced the final passing native gate.

## Live candidate Workflow validation between Steps 16 and 17

On 2026-10-12, the operator successfully retried [Print a random quote after the greeting, Issue 13](https://github.com/jozala/omnigrex-testrun/issues/13) using Pi on a Linux/amd64 Microsandbox candidate deployment.
The Developer implemented and published [Change Proposal PR 14](https://github.com/jozala/omnigrex-testrun/pull/14), and the separate Omnigrex Reviewer App submitted an `APPROVED` review at 23:20:28 UTC for head `411b75d9edb419e7e8a9ca439c55946ba9388640`.
Both the Issue and Pull Request carried `omnigrex:pr-ready` when the outcome was verified; the Pull Request remained open for human review and merge.

The successful retry followed repository Go toolchain declaration through mise and deployment of the Pi startup-readiness fix in commit `0755fa273aee2fb8a4d24a7aa9d51233c929d2de`.
Earlier attempts had stopped for unavailable Go tools and then exhausted infrastructure retries during Session preparation.
The [startup-readiness corrective plan](2026-10-12-pi-startup-readiness-budget.md) describes the observed bootstrap/deadline coupling and proposed separation of startup and ordinary RPC budgets.

The Reviewer reported successful `gofmt -d main.go`, `go test ./...`, `go vet ./...`, `go build`, and smoke checks with a supplied name and with no input.
The `go test` result established package compilation, with no test files present; the smoke checks confirmed one supplied quote and its matching author after each greeting path.
The Reviewer also reported a clean merge tree and no blocking findings.
These commands are recorded as Reviewer-reported verification, not as independently rerun checks by the documentation author.

This is live candidate evidence for the Step 16 operator procedure's Developer publication, Reviewer approval, and human review readiness, and early evidence for Step 17's isolated Microsandbox dogfooding requirement.
The trial used manually published candidate images and does not replace merged-release publication, public-pull qualification, exact-digest deployment/cutover rehearsal, coordinated backup/restore verification, or final qualification after OpenCode removal.
This small Go Work Item did not exercise Docker, Compose, or Testcontainers inside the guest.
The outstanding release and final-cutover boundaries remain as described above.

## Independent review corrections

Round 1 used separate Standards and Spec subagents over the entire Step 16 working tree against pre-Step-16 HEAD, including every untracked addition.
It found three distinct issues: native restore replaced protected state without first confirming empty native inventory, mixed published-target/source-built image selection used an uninitialized registry port, and explicit Compose file arguments discarded the operator's selected overrides.

Restore now validates backend and required recovery inputs before changes, stops the control plane, and requires successful empty native inventory before replacing either retained-state storage or the dispatch journal.
Nonempty and unavailable native inventories leave the current protected data untouched.
One idempotent fixture-registry owner serves both source and target publication, and source builds use the target guest platform.
Deployment, native image provisioning, diagnostics, and instruction reload examples consistently inherit the exported complete Compose file set.

The mixed-image and authoritative restore-example regressions failed before the corrections and passed afterward.
Coverage now includes all eight source/target/backend image-selection combinations, the existing four cutover failure barriers, three native restore-inventory cases, and actual documentation command prefixes rendered through Compose with selected storage/instruction overrides on both backends.
The focused race-enabled regressions, `mise run check`, `mise run test-integration`, and both full source-host cutover rehearsals passed after the corrections.

Round 2 used two fresh independent Standards and Spec subagents to review the entire corrected tracked and untracked Step 16 diff.
Both confirmed all original findings resolved and reported zero new actionable findings or remaining important P1/P2 findings.
Their independent checks reran the script/restore regressions and both-backend documentation Compose rendering, and the Spec reviewer also checked shell syntax and original Work Item reconciliation.
Round 3 used another fresh Standards/Spec pair to independently confirm the complete final tracked and untracked change set.
Both again reported all original issues resolved, zero important P1/P2 findings, and no new actionable findings.
Their read-only checks confirmed shell syntax, immutable review scope, selected Compose overlays, original Work Item reconciliation, and the still-pending main-branch Pi release boundary.
These clean source-review results do not establish the outstanding published-release gate described above.

## Original Work Item reconciliation

The combined plan governs the sequential delivery of [Replace OpenCode with Pi RPC as the sole agent runtime](https://github.com/jozala/omnigrex/issues/33) and [Migrate Agent Turn isolation from Docker containers to Microsandbox](https://app.clickup.com/t/124ckhdev73).
Their descriptions were reconciled at this milestone to preserve the original requirements and identify the superseding scope and then-remaining release boundary.
The current [Step 17 release record](2026-10-13-pi-only-final-qualification.md#published-release-and-operator-cutover) supersedes the delivery-status column below where publication or cutover was still outstanding.

| Original requirement or assumption | Delivered scope and qualification |
| --- | --- |
| Docker isolation is replaced outright | Docker is retained for trusted repositories; one selected Linux/KVM Microsandbox implementation supplies stronger isolation and nested-container support. |
| Pi uses the existing Docker process transport | Pi consumes neutral non-TTY process transport on both backends; core coordination and MCP policy stay backend-independent. |
| One deployment image and persistent assignment directory | New Participants independently select digest-pinned golden-derived environments; exact retained contracts and logical Participant/Session state ownership remain immutable. |
| Blanket disablement of repository skills | Superseded by decisions 0003/0006/0014 and ADR 0012: explicitly authorized current-checkout skills remain available, while uncontrolled project resources stay disabled. |
| OpenCode state can be reset at replacement | Explicit stopped-state backup and clean disposal are documented and rehearsed; no conversation or populated operational-ledger conversion is implemented. |
| Future Pi upgrades preserve retained Sessions | Backend-scoped source-to-candidate continuation/replay on copies qualifies exact contracts; qualification never rebinds retained Participants. |
| Sandbox credentials may replace a provider proxy | The investigation does not establish mediation; real selected-provider API keys are ephemeral on both backends, and provider mediation/OAuth refresh remain deferred. |
| Repository images and additional tools | Images are built externally and verified by actual golden resources; both Roles install working-revision tools inside execution, separate from trusted Profile provenance. |
| Docker/Compose/Testcontainers and Omnigrex integration tests | Qualified inside Microsandbox using its private guest daemon; host Docker authority is never delivered to the guest. |
| Nested microVM tests | Investigated only, not a release requirement or full feature-parity claim. |
| Live provider examples | Step 02 records live API-key feasibility; this rehearsal uses controlled providers and does not claim new live-provider qualification. |
| Pi-only release, no-clone published deployment, dogfooding | Source bundles and fixtures are qualified; merged publication/public pulls/exact-digest rehearsal and Step 17 final cutover remain outstanding. |

The original Work Items remain open until their remaining release/final-cutover requirements pass.
The retained OpenCode implementation, legacy bundles, old Profile schema, and old image publication were transitional and removed in Step 17.
The rehearsal's default legacy source-image build and copy of `deploy/compose.yaml` were also preparatory dependencies.
Step 17 removed that source-build fallback; the current operator rehearsal requires an externally selected old-release image-only bundle and exact published source-image pair, so Pi-only source does not depend on deleted OpenCode files.

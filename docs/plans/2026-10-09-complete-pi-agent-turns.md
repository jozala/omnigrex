# Complete Pi Agent Turns on both backends

## Prerequisite and scope

This implements Step 12 of the [combined plan](2026-10-05-pi-and-pluggable-execution-backends.md), following [Step 11 durable Sessions](2026-10-09-durable-pi-sessions.md).
It applies the [execution and agent contracts](2026-10-07-runtime-boundary-contracts.md), [decision 0016](../decisions/0016-qualify-pi-turns-with-existing-settlement-policy.md), and existing Role, MCP, publication, and settlement policies.

## Composition and candidate configuration

`internal/runtime/profile.NewPiV1` supplies a strict immutable `pi-rpc/v1` contract with an exact registry image, supported Linux platform, qualified Node/runner command, non-root identity, logical attachments, and bounded disposable paths.
The `/state` attachment contains `pi-session/v3` data, while `/run/pi-agent` contains disposable provider inputs.
Existing installation/cache guest paths remain common with the environment boundary; they do not select the conversational runtime or imply shared host access.

Composition registers the Pi contract when optional candidate image/platform settings are supplied and constructs `pi.TurnAdapter` with the narrow durable Session authority interface.
The candidate is discoverable through explicit Pi repository Profiles and the existing preparation service.
Current image selection and retained immutable bindings continue following decision 0009.
Both Compose bundles pass the optional settings without changing the current OpenCode default.
Candidate qualification and credential placement procedures are authoritative in the [operator guide](../operator-guide.md#candidate-pi-agent-turn-qualification).

## Launch, prompt, and settlement ownership

`internal/runtime/session.TurnAdapter` separates agent process/configuration preparation from the common Launcher.
Pi compiles a credential-free execution request and renders the validated durable Profile snapshot using current platform/operator/Stage instructions and exact per-Turn MCP authority.
The existing Launcher prepares the working checkout and tool paths, admits state, registers authenticated MCP, starts the selected backend, and calls the Step 11 fenced coordinator without submitting a prompt.
Repository tooling still runs through the qualified in-environment bootstrap, with loader variables and runtime configuration protected from repository activation.

The execution Worker submits the same structured event envelope through the neutral Turn connection under a live prompt fence and finite deadline.
An unavailable adapter fails explicitly instead of requesting another agent runtime.
Pi's reader retains the admission-versus-settlement distinction and emits safe Agent Events through the application sink.

The common finalization path closes mutation admission, drains authenticated MCP, confirms environment-wide stop, seals terminal workspace evidence, removes execution resources, and applies Role-specific scratch/workspace cleanup.
Reviewer workspaces are discarded only after the required observations.
Trusted publication, multi-commit validation, expected remote-head checks, GitHub credential selection, review signatures, and outcome reconciliation remain the existing implementations.

A temporary compatibility shim translates neutral settled completion, token limits, aborts, failures, and uncertain transport outcomes into the existing durable ending vocabulary.
Unconfirmed admission cannot become completion, and reaching a token limit cannot become a normal handoff.
Unknown prompt execution remains distinct from confirmed environment termination.
The ACP-named surfaces remain only for preparation-stage compatibility and are removed in Step 17.

## Golden environment and resource qualification

`agent/pi/Dockerfile` defines a non-root candidate environment with Pi 1.0.4, Node 24.14.1, locked npm dependencies, digest-pinned Debian toolchain/base content, checksum-locked mise/ripgrep/fd, Git, and the owned runner resources.
It retains the integrity-locked Pi distribution for offline resource validation.
The trusted environment qualification checks the locked package archives' SHA-512, compares each installed package's complete file inventory with those archives, verifies the Node version and mise checksum, and verifies owned JavaScript and canonical npm manifests against checksums embedded in the control plane.
Package metadata and added executable files are covered, required dependencies cannot disappear, and unqualified dependency shadows or symlink aliases are rejected.
Docker preparation and native Microsandbox image qualification invoke the same resource contract before repository scripts.
The production `golden` target excludes the controlled provider entry point; the separate `workflow-fixture` target adds it for qualification.

## Controlled Work Item acceptance

One shared test runs separately through Docker and the pinned native Microsandbox SDK on Linux/KVM.
It starts a disposable PostgreSQL service and independent namespaced storage/network resources, creates a real Git seed and remote, admits a Work Item through the normal Store transition, and prepares Developer then Reviewer through the production Profile loader and Preparer.
The fixture's HTTP model drives actual Pi tool calls through the production owned runtime and authenticated MCP extension.
Developer creates two commits, publishes through the trusted capture/publication service, opens the Change Proposal, and requests review.
Reviewer reads the Change Proposal and submits one signed native approval using Reviewer authority.
Both Agent Turns pass the normal capture/cleanup and outcome settlement path, producing `DEVELOPING -> REVIEWING -> PR_READY`.

The gate verifies distinct durable Pi Session identities, absence of fixture provider/GitHub credentials in retained Session files, Reviewer tool/MCP restrictions, Reviewer workspace discard, one opened Change Proposal, and one approved review.
GitHub uses a controlled API fixture; trusted Git operations use a credential-free local remote rewrite.
No external GitHub mutation or live provider call occurs.
The fixture entry point supplies only the controlled HTTP model catalog and delegates configuration, persistence, RPC, and tool authority to the production runner.
Docker qualification uses a content-addressed local fixture image with a narrow image alias at the backend/state metadata fixture boundary; production admission continues requiring actual registry digests.
Microsandbox imports and verifies the exact fixture manifest through its normal image boundary.
The outer Docker qualification control plane can access the daemon socket; the Pi environment receives only its scoped attachments and never that socket.

## Verification evidence

Observed passing commands:

```sh
mise run check
mise exec -- go test -race ./internal/agentturn -run TestExecutionWorkerPreservesNeutralPromptEndings -count=1
mise run test-integration
mise run test-microsandbox-integration
mise run test-pi-integration
mise run test-pi-microsandbox-integration
mise run test-pi-workflow
mise run test-pi-microsandbox-workflow
```

The remote native gates used the explicitly approved Linux/KVM Docker host and numeric KVM group; connection values and deployment metadata are excluded here.
The first Workflow runs exposed fixture-only gaps in registry loopback addressing, repository identity consistency, native image-cache initialization, and empty review-file observations.
The corrected gate retains all production boundaries and passed on both backends.
Public Worker tests cover normal completion, token limits, aborts, provider failure, lost settlement, and unconfirmed admission.
The existing Docker integration suite and both complete preparatory Pi suites passed with the candidate wiring.
The full native Microsandbox conformance gate also passed, including retained OpenCode golden-environment compatibility and nested Docker/Compose/Testcontainers qualification.

### Review and final verification

Both independent Standards and Spec sub-agent requests failed before producing a review because the agent usage limit had been reached.
This milestone therefore does not claim independent review approval.
A manual Standards and Spec review inspected the complete forty-six-file diff against the Step 11 baseline `d735f21`, tracing each Step 12 scope item through composition, launch, prompt fencing, publication, capture/cleanup, and settlement.
The review strengthened the Workflow gate's native review assertion to count actual submission calls explicitly and pinned the disposable PostgreSQL fixture by digest.
Both complete Workflow gates and the normal repository check passed again after those final changes.
The existing Docker integration suite, both preparatory Pi suites, and full native backend conformance evidence apply to the final production implementation; subsequent changes were confined to qualification assertions, the disposable database image pin, and documentation.

### Independent review corrections

A subsequent independent review of `d7af8dd` reported two validated P2 defects: SDK export redirection passing resource qualification, and valid repository Profiles being rejected after JSON snapshot expansion.
Both were reproduced with permanent failing tests before correction.
The canonical npm manifests now belong to `agent/pi/`, and the golden image retains integrity-locked archives for every installed runtime dependency.
A builtin-only offline validator checks package metadata and complete file inventories without importing the packages under qualification or npm's own parser.
Regressions cover SDK and dependency export redirection, added modules, missing required dependencies, and nested dependency shadows.
Snapshots now have an encoding-aware bound and share semantic Profile validation with repository parsing instead of reconstructing YAML.
Round-trip tests preserve escaped instructions near the source-size bound; decoded content remains bounded and Pi's presence-sensitive schema exclusions remain enforced.
The bootstrap encoder avoids unnecessary HTML escaping so these valid instructions also fit the existing JSONL record budget.
The two non-blocking Standards findings were addressed by an explicit runtime qualification parameter and a caller-deadline-only prompt policy, replacing the one-year duration sentinel.
Verification also reproduced an IPv4/IPv6 mismatch in the diagnostic MCP test fixture; that fixture now targets the IPv4 address family its listener actually binds.
The first correction review confirmed both original P2 findings resolved and found one additional P2: a byte-identical runner symlink could change Node's physical dependency-resolution directory while passing checksums.
A permanent runner-alias regression reproduced the bypass before the correction.
Qualification now requires physical, non-symlink resource ancestor directories and regular non-symlink owned files before hashing or executing the offline validator.
The final independent Standards review reported zero documented breaches or actionable heuristics, and the final independent Spec/correctness review reported zero new or unresolved worthwhile findings.
The correctness reviewer independently rejected aliases for the runner, validator, package-integrity module, both npm manifests, and each resource ancestor, while unchanged and restored positive controls passed.
The final public resource gate contains one positive control and seven negative scenarios, and both complete controlled Workflow gates passed with the final path guards.
The normal repository check, focused race suites, both preparatory Pi backend suites, and experiment image build also passed during this correction milestone.
An initial legacy Docker integration run failed because Docker Desktop's disk was full; explicitly approved unused build-cache cleanup preserved images, containers, and volumes, and the complete integration gate then passed on the final code.

### Approved disk-backed scratch regression

A subsequent human review reproduced an approved `TMPDIR` path conflicting with Pi's immutable Profile default `/tmp` before environment creation.
The public adapter regression and the controlled Docker Workflow both reproduced that launch failure before correction.
Pi now treats the Profile's `TMPDIR` value as an unconfigured fallback and preserves an admitted control-plane binding, while other fixed runtime settings remain protected and the immutable Profile binding stays unchanged.
The complete Workflow fixture now discovers the same `TMPDIR`, `GOTMPDIR`, `GOCACHE`, and `GOPATH` directory/lifecycle requests as this repository's Turn configuration under an explicit operator allowlist.
Both Developer and Reviewer processes verify exact guest path values and prepared directories through the production bootstrap, and the gate verifies Turn scratch removal while assignment caches remain available.
The focused race suites, normal repository check, complete Docker integration suite, and both controlled Workflow gates passed after the correction.
Independent Standards and Spec/correctness reviews both reported zero worthwhile findings and confirmed the P2 resolved.
The correctness reviewer independently exercised the repository's actual Turn configuration through admission, fenced path preparation, Pi compilation, conflicting mise activation, and cleanup, confirming invalid admission and unsafe values fail while approved bindings and cache lifetimes are preserved.

## Next milestone

Step 13 expands qualification to the full restart, transport-loss, stale-lease, mutation-ambiguity, and cleanup fault matrix.
Pi upgrade qualification, published operational artifacts, selected-backend doctor completion, and the operator-controlled clean cutover retain their assigned later steps.

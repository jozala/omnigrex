# Repository environments and in-environment tooling

## Scope and prerequisite

This delivers Step 07 of the [combined implementation plan](2026-10-05-pi-and-pluggable-execution-backends.md), following [Step 06](2026-10-08-retained-state-bindings.md).
It implements [decision 0009](../decisions/0009-pin-participant-images-and-use-working-revision-tools.md) and the repository environment section of the [boundary contracts](2026-10-07-runtime-boundary-contracts.md).
[Decision 0012](../decisions/0012-qualify-repository-environments-without-rebinding-sessions.md) records resource qualification, durable contract content, and the separation of fresh Participant admission from Session migration.
OpenCode remains the active agent runtime during this preparatory step.

## Configuration and immutable selection

`.omnigrex/environment.yaml` is an optional strict, single-document YAML file with a 64 KiB maximum.
Version 1 requires a canonical, untagged registry reference pinned by SHA-256 and either `linux/amd64` or `linux/arm64`.
Its optional `requirements` list accepts `process-isolation`, `hardware-isolation`, `nested-containers`, and `tool-installation`, rejecting unknown and duplicate entries.
The file cannot request backend selection, host mounts, arbitrary path values, devices, or credentials.

The Agent Profile loader reads this file from the same exact default-branch commit as the Agent Profiles and Turn path configuration.
An absent file selects the deployment golden image.
Each new Participant binds its own image, platform, environment contract version, and requirements atomically with Participant and Agent Session preparation.
A later Reviewer can select a different current image within the same Assignment Generation.
Fresh Participants use independent state and do not require a cross-image Session upgrade result merely because another Participant previously used a different image.
Their exact runtime resources are qualified before repository scripts execute; candidate Session qualification remains a separate operation on copied source state.
An existing Participant resolves its original contract regardless of a newer repository or deployment image.
Agent Profile instructions, model preferences, and their default-branch provenance remain separate from working-revision tooling.

Migration `000033_repository_environment_contracts.sql` stores full immutable canonical Runtime Profile definitions by content hash.
Resolution validates stored content against its complete binding and fails on corruption or storage failure.
Canonical definitions are included in the coordinated PostgreSQL backup; custom-image recovery does not infer configuration from a mutable current default.
Historical built-in contracts remain resolvable by their exact hashes for the transitional OpenCode path.
Retained pre-Step07 contracts also receive in-environment provisioning and activation without changing their Runtime Profile hash, pinned image, or Agent Session identity.
Their exact execution request names the deployment golden image as a bootstrap artifact source; the trusted managed initializer qualifies that source and copies only the bootstrap into fenced Turn-local tool scratch.
The retained environment mounts that directory read-only and runs tool installation and the original absolute OpenCode executable inside its original image.
The source image digest participates in execution reservation identity, and the staged bootstrap is collected with exact Turn scratch only after confirmed environment removal.
Missing bootstrap configuration or unavailable source resources fails explicitly rather than launching without tools.
The earlier deployment-wide candidate admission guard remains only for legacy callers without the versioned repository environment contract and is removed with OpenCode in Step 17.

## Golden-image resources

The current golden image is built by `agent/opencode/Dockerfile`.
It includes the non-root UID/GID `10001:10001`, absolute OpenCode and mise executables, Git and shell utilities, the versioned environment bootstrap, and the original pinned OpenCode distribution archive.
The Docker adapter verifies the pinned mise checksum and OpenCode archive checksum, reconstructs and compares the OpenCode executable checksum, and checks bootstrap contract identity and required executables before repository setup.
The diagnostic path performs the same qualification with no workspace, retained state, network access, or credentials.
Image ancestry, labels, and a claimed runtime version alone are insufficient.
Omnigrex never builds or automatically pulls repository images.
The exact selected platform and digest must already be available to the execution backend.

The original Alpine-based example has been retired; the current [Pi repository image example](../../agent/environments/pi-example/Dockerfile) derives from an externally supplied Pi golden digest and adds an explicitly versioned Debian development package.
Image construction, publication, availability, and configuration procedures are authoritative in the [operator guide](../operator-guide.md#repository-environments).

## Tool setup and control protection

The launcher prepares the working checkout and path attachments, then launches `/usr/local/bin/omnigrex-environment run acp` in the managed environment.
The bootstrap installs tools with the pinned absolute mise executable from that checkout for both Roles, obtains bounded JSON activation, and replaces itself with the absolute OpenCode executable.
No repository mise command or installation script runs in the orchestrator, which no longer contains mise.
Installation output cannot contaminate protocol stdout or become unbounded credential-bearing diagnostics.
Failures and cancellation are explicit; subprocess output waits are bounded.

The bootstrap filters image environment entries against the control plane's admitted variable names, clears loader injection settings, and rejects repository activation that changes protected runtime variables.
Admitted control-plane values, including all approved cache and scratch path bindings, remain authoritative over mise activation.
Repository activation can populate safe variables only where the control plane has not assigned them; it cannot redirect `GOCACHE`, `GOPATH`, or another allocated directory to a different lifecycle or location.
Tool activation cannot replace `PI_*`, `OMNIGREX_*`, `OPENCODE_*`, loader settings, or protected home/configuration variables.
The absolute agent executable cannot be shadowed through tool `PATH` activation.
Runtime automatic mise discovery is disabled after activation.
Installation subprocesses receive an isolated tool environment without provider or MCP configuration entries.
The existing trusted-repository model still applies: repository code runs in an environment that receives real provider credentials, and clearing subprocess environment entries is not credential mediation.

Tool installation data is replaced each Turn under its Participant-local mise attachment.
Installation scratch is removed before agent startup.
Separately attached Turn scratch and Participant tool caches retain their existing cleanup and collection rules.
Retained Runtime State, workspace data, installation data, and tool caches remain distinct resources.
Reviewer local-tool restrictions, MCP authority, and workspace discard remain enforced.

## Backend admission and recovery

The selected backend validates required capabilities before checkout preparation, MCP registration, or repository script execution.
Docker supports process isolation and in-environment tool installation; hardware isolation and nested containers fail clearly without host-socket fallback.
Tool setup belongs to the managed execution epoch, so existing exact-key inventory, deadlines, creation reservations, and environment-wide stop cover setup and its background workloads.
Closing protocol transport is still not stop evidence.

The new bootstrap is a transitional OpenCode launcher, with OpenCode-specific executable/resource qualification replaced during the Pi image and adapter steps and removed in Step 17.
The neutral environment selection, contract persistence, capability admission, working-revision installation, and storage ownership remain applicable to Pi.

## Acceptance evidence

- Unit coverage exercises strict image configuration, default-branch provenance, per-Participant pinning, a later Reviewer selecting a newer image, retained contract resolution, corruption detection, protected activation variables, loader filtering, bounded output, and cancellation.
- Launcher regression coverage checks working-revision environment launch for both Roles and rejection of unsupported Docker capabilities before repository work.
- Docker integration builds a separate repository image with an additional development tool, qualifies its actual resources, and runs a working-checkout mise installation hook as UID 10001 before successful ACP initialization for both Roles.
- The integration observes the installation artifact and activated working-revision variable inside the guest, rejects an unrelated image without golden resources, and rejects a derived image that replaces the pinned runtime executable while retaining its version claim.
- PostgreSQL integration verifies atomic contract publication with Participant preparation, rejection of malformed content, exact re-resolution, database-enforced immutability, and a second independent image's admission without changing the original Participant or Session.
- Tool-data regression coverage replaces restrictive installation directories and removes restrictive setup scratch while preserving neighboring state, workspace, caches, and external symlink targets.
- Upgrade regression coverage resolves a pre-Step07 Participant through the current catalog, stages a read-only bootstrap for its next Turn, and verifies the retained image, Runtime Profile hash, Session identity, and exact scratch cleanup.
- Uncached Docker coverage creates a real Session in an image without the Step07 bootstrap, installs the next working revision's tool and activated `PATH` inside that same image, and continues the original Session without a model prompt.
- Final bootstrap and guest-process environment checks preserve control-plane `GOCACHE`, `GOPATH`, and another approved cache variable against conflicting mise activation.

The final local gate and Docker-backed integration gate passed on Docker Engine 29.8.1 with the development `linux/arm64` golden image.
The local gate passed formatting, static analysis, production reachability, race-detected unit tests, dependency checks, and both Compose configuration validations.
The integration gate passed doctor, OpenCode ACP, Docker, PostgreSQL Store, and deployment suites, including the new repository environment and immutable contract scenarios.

The following commands passed during implementation and final verification:

```sh
mise exec -- go test -race ./internal/runtime/environment ./cmd/omnigrex-environment
mise exec -- go test -race -tags=integration ./internal/store -run 'RuntimeProfile|Compatibility|RepositoryRuntimeContract' -count=1 -timeout 5m
mise exec -- go test -race -tags=integration ./internal/runtime/docker -run '^TestRepositoryEnvironment' -count=1 -timeout 3m
mise exec -- go test -race -tags=integration ./internal/runtime/docker -run '^Test(RetainedRepositoryEnvironment|RepositoryEnvironment)' -count=1 -v -timeout 5m
mise run check
mise run test-integration
git diff --check
```

The first full integration attempt exposed six assertions still expecting schema version 32; those assertions now cover version 33 and its exact migration checksum and table, and the final gate passes.
The destructive Compose resource-recreation test was not run.
Step 08 repeats the environment contract and working-revision tooling qualification through the Microsandbox implementation.

### Post-review regression verification

The review identified a retained-contract provisioning bypass and mise activation overwriting control-plane cache bindings.
`TestRetainedPreStep07ParticipantProvisionsToolsWithoutRebinding` and `TestBootstrapPreservesAdmittedCachePathsAfterMiseActivation` reproduced those failures before correction and passed with `-count=1` after correction.
`TestRetainedRepositoryEnvironmentContinuesSessionAndWorkingRevisionTools` passed uncached against a real retained-image fixture without the Step07 bootstrap.
It continued the original ACP Session, installed and executed the next working revision's tool through its activated `PATH`, preserved admitted `GOCACHE` and `GOPATH` in the actual running process, and verified the original image and contract binding.
The final post-review `mise run check`, `mise run test-integration`, and `git diff --check` passed.
Doctor, ACP, Docker, and Store integration packages executed again after the corrections; deployment reused its valid cached result.

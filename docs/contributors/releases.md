# Publish a product release

[Contributor testing](testing.md) · [Installation](../operators/installation.md)

Releases designate existing qualified builds; they do not deploy anything.
The accepted [release policy](../decisions/0054-manual-product-releases.md) is pre-1.0: stable means recommended for users, not production-ready or permanently backward-compatible.
Only Linux/amd64 is supported.

## Select an exact candidate

Choose an explicit **CI** run ID from a successful `push` to `main`, not a PR run, unrelated green check, workflow summary, mutable `sha-*` image tag, or current main tip.
All existing jobs must pass, including `check`, development qualification, both exact-digest deployment smoke jobs, and development Microsandbox smoke.
`publish` records both actual pushed orchestrator digests and the deliberately selected exact Pi digest and builds `deployment-payload-RUN-ATTEMPT`.
It also publishes the development image against that selected Pi base, but does not publish a new Pi candidate.
Use the separate [explicit candidate build and qualification dispatches](runtime-qualification.md#qualify-an-unselected-bundle-before-adoption) when preparing a new runtime; neither dispatch adopts it.
Both product smoke jobs verify and extract that payload and use its packaged Compose files with those captured digests.
Only afterward does `release-candidate` record job IDs and upload `release-candidate-RUN-ATTEMPT` with a machine-readable `candidate.json`, deployment payload, and checksums.
Main CI creates no version tag or GitHub Release.

The early **Build/publication output — not deployment approval** summary separates selected production Pi from any newly published, unselected Pi candidate.
Changing `.env` alone does not adopt a candidate or change the approved runtime resource inventory.
After `ready` verifies the payload and all required successful job identities, `release-candidate` renders **Qualified main CI candidate — deployment configuration** in its job summary and the separate `deployment-guidance-RUN-ATTEMPT` artifact (14-day retention).
It contains backend-specific non-secret `.env` snippets and the existing qualified `development-environment` YAML, independently adopted for this repository only.
The renderer consumes verified packaged `build.json`/candidate metadata, not current main or the last built Pi candidate; it does not require a newly published Pi candidate.
Failed, cancelled, skipped, incomplete, or missing required qualification produces no ready guidance.
This derived presentation is not another selection record or a new release asset; archives, checksums, designation, and byte-preserving promotion remain unchanged.
For local fixture inspection, `go run ./scripts/release guidance CANDIDATE_DIRECTORY QUALIFIED_DEVELOPMENT_YAML` prints the same Markdown and rejects missing readiness evidence or invalid inputs.
Operators do not need a checkout or this command: use the source run's summary/artifact and packaged release metadata as described in [installation](../operators/installation.md#3-resolve-and-provision-images).

Schema-2 payloads capture the complete selected `pi_bundle` with its original source commit and qualification run/attempt/job IDs, separately from orchestrator build identity.
Packaging resolves Pi from the checked-in selection, not a mutable tag or the newly published Pi candidate.
Both exact-digest product smokes qualify the current orchestrator with that selected Pi.
Original Pi qualification provenance is also checked independently when marking a candidate ready and designating an alpha.
Schema-1 assets remain verifiable and promotable without changing their bytes or inventing missing provenance.

New deployment payloads include `LICENSE`, `THIRD_PARTY_NOTICES.md`, and the explicitly allowlisted third-party license files from `scripts/release/artifacts.go`.
Both orchestrator images carry the same notices and license tree under `/usr/share/licenses/omnigrex/`.
Keep these files with redistributed artifacts; see [Third-party notices](../../THIRD_PARTY_NOTICES.md) for attribution and affected files.
Previously qualified payloads without separate notices remain verifiable and promotable without repacking.

The release workflow verifies the run's repository, workflow ID/path, push event, main branch ancestry, exact commit, latest attempt, successful conclusion, and required job IDs against the manifest.
Required artifacts have 14-day retention; unavailable, expired, skipped, failed, cancelled, or incomplete evidence is not eligible.
Older qualified main commits are allowed while this evidence exists.
If a run is rerun, use **Re-run all jobs** so every required job and payload belongs to that attempt.
A partial rerun cannot reuse old qualification evidence; it fails closed.
A rerun can republish commit image tags; release operations never resolve those tags.
After evidence expires, select a newly qualified build rather than bypassing validation or rebuilding inside a release workflow.

## Publish an alpha

In Actions, select **Publish alpha**, **Run workflow**, and branch **main**.
Supply:

- `source_run`: the exact selected CI run ID.
- `version`: full version without `v`, initially `0.1.0-alpha.1`.
- `notes`: human-written release notes.
- `breaking`: explicit breaking changes, or `None`.
- `upgrades`: manual prerequisites, a fresh-install requirement, or explicit `None`.
  State untested historical upgrade paths and compatibility unknowns honestly.
  Fresh-install smoke does not establish every upgrade path.

The workflow adds the `v` tag prefix and creates a public GitHub prerelease, explicitly not GitHub's recommended latest stable release.
It reserves an exact lightweight tag without force, creates a draft, uploads assets, downloads and compares their bytes, verifies the tag, then publishes.
Product images are **orchestrator**, **orchestrator-microsandbox**, and generic **pi**, all pinned by digest in the bundle's `build.json`.
The orchestrator version remains its source commit.
`release.json` carries the designation, channel, breaking-change/upgrade declarations, commit, and payload checksum separately.
No release operation builds images, alters image labels, creates `latest`, changes `.omnigrex/environment.yaml`, or rebinds retained Agent Participants.
The Omnigrex-specific `development` image remains independently qualified and adopted by a separate repository change; other repositories supply their own environments, optionally derived from Pi.

A Pi/development-image update alone is not a breaking change.
Declare coordinated selection/repository-image migration in upgrade prerequisites, and assess actual Session, protocol, API, configuration and other incompatibilities independently.
Ordinary runtime updates retain existing Participants and do not inherently require them to finish first.
An ordinary orchestrator-only upgrade keeps the selection, installation Pi digest and repository pins unchanged.
See the [operator cutover](../operators/upgrades.md#runtime-profile-image-change).

Alpha sequence numbers must increase within a stable target.
Judge compatibility against the previous **stable**, not the previous alpha.
After stable `0.1.0`, a compatible patch may target `0.1.1-alpha.1`; declaring a break requires `0.2.0-alpha.1` instead.
The same candidate may receive that new designation without rebuilding; the earlier alpha is preserved.
Further breaks during unreleased `0.2.0-alpha.N` do not inherently require `0.3.0`.
Stable versions move forward only; there is one current stable line, with no historical backports.

## Promote to stable

Select **Promote to stable**, branch **main**, with `alpha` such as `0.1.0-alpha.1`, `version` exactly `0.1.0`, and finalized human notes.
Promotion only removes the selected alpha suffix; it cannot retarget to another stable version.
It reads the retained alpha Release assets and tag, not Actions artifacts or commit image tags.
It creates a separate normal Release/tag at the same commit, retaining the alpha.
`payload.tar.gz`, `candidate.json`, and `SHA256SUMS` remain byte-identical; only `release.json` and notes change.
No expensive backend suite or image build is repeated.
Releases and their assets are retained indefinitely; never prune exact images merely because a newer release exists.

## One-time dedicated release App setup

Mariusz approved this authorization amendment in [Issue #160](https://github.com/jozala/omnigrex/issues/160#issuecomment-6550576581).
His supplied live probe found that the ordinary workflow token could create a current-tip ref but received HTTP 403 for an unreferenced older main ancestor with different workflow contents.
The failed response advertised Contents/Workflows write as an accepted permission combination.
This is maintainer-supplied evidence for that tested historical case, not a claim that every older commit fails or that the App path has already been live-verified.
Adding `workflows: write` to `GITHUB_TOKEN` YAML permissions cannot grant this authority.

Before the first live release, Mariusz or an administrator must:

1. Create a **dedicated release GitHub App** owned by the repository owner, separate from the orchestrator's Developer and Reviewer Apps.
   Give it repository **Contents: read and write**, **Workflows: read and write**, and GitHub's mandatory **Metadata: read** permissions only.
   It needs no organization permissions, webhook subscriptions, or orchestrator/deployment access; disable its webhook if unused.
2. Install it with **Only select repositories**, selecting only `jozala/omnigrex`.
   Approve the installation's requested permissions, including Workflows write; after future permission changes explicitly review/approve the installation update.
3. Record the App's numeric **App ID** (not client ID or installation ID) as repository Actions variable `OMNIGREX_RELEASE_APP_ID`.
   Generate/download a private key and store the complete PEM as repository Actions secret `OMNIGREX_RELEASE_APP_PRIVATE_KEY`.
   Keep the private key out of source, release assets, logs, deployment configuration, and documentation.
   Never reuse the orchestrator's existing App keys or introduce a PAT.
4. Keep repository workflow/secret access limited to trusted maintainers.
   The App's Workflows-write authority is consequential even though this command uses it only for tags.
   Rotate the private key by replacing the Actions secret and revoking the old App key; uninstall/revoke the dedicated App if release authorization is withdrawn.

At publication, the Go helper signs a short-lived App JWT, locates this repository's installation, and requests an installation token restricted to `omnigrex` with Contents/Workflows write.
It validates the returned repository scope, permissions, and expiry, creates only the selected exact lightweight tag without force, and revokes the token on success or failure.
Tokens expire within one hour even if runner termination prevents revocation.
The normal workflow token performs all remaining GitHub operations; the App token is never passed to `gh release upload`, stored as an artifact, or used as a fallback credential for other operations.
Missing configuration, permission rejection, or a scope mismatch fails clearly; it never selects a different commit, substitutes image tags, or rebuilds.

Both release workflows are manual and main-only; CI push events select only the main branch.
Unlike `GITHUB_TOKEN`, App-created tags can trigger Actions, so do not add tag-triggered build/release jobs without reviewing this boundary.
App provisioning and actual historical-tag permission verification are external acceptance, not prerequisites for implementing fixture tests.

## Failure and manual retry

Both entry points serialize through one concurrency group and never cancel an in-progress publication.
They use the Actions token with job-scoped `contents: write` and `actions: read` for evidence, Release/assets, and tag reads.
Only exact tag creation uses a short-lived, repository-scoped token from the dedicated release GitHub App described below.
No PAT or approval environment is required.
A duplicate version, draft, or existing tag fails rather than being overwritten or moved.
If upload/verification fails, inspect the incomplete draft and any associated tag manually.
Delete only the unpublished draft and its unused tag after confirming no published release or retained consumer depends on it, then rerun with the same inputs.
Never delete or retarget published versions to make a retry pass.
There is no automatic resume, cleanup, deployment, or package-visibility administration.

## Verification and first live acceptance

The release command and ordinary Go behavior tests live under `scripts/release/` and run through `go test ./...` / `mise run check`.
Focused verification is `mise exec -- go test -race ./scripts/release`.
Fixtures cover qualification/provenance failures, versions, archive validation, no-checkout Compose, redesignation, byte-preserving promotion with expired CI artifacts, and rejection/successful routing of historical tag authorization.
They are not independent proof of live GitHub permission behavior.
Run `mise exec -- go test -race ./.github/workflows` explicitly (hidden directories are excluded by `go test ./...`).
Use `mise run check` and `mise run test-integration`; KVM qualification remains in CI.

After merge and a new main candidate qualifies, **Mariusz or an explicitly authorized external agent** must:

1. Provision and approve the dedicated release App, confirm both manual workflows appear, and verify actual historical tag creation for a qualified older main commit whose workflows differ from current main and which has no direct branch/tag reference.
   This requires explicit authorization for a live tag; fixture tests alone do not establish that the App permissions work.
2. Intentionally publish `v0.1.0-alpha.1` from an explicitly selected qualified run.
3. Download and checksum the assets without a checkout, configure both overlays, and verify anonymous pulls of all product images.
   Existing post-publication smoke logs into GHCR and does not prove anonymous availability; package visibility is an administrator responsibility.
4. When intentionally approved, promote to `v0.1.0` and compare payload checksums and exact image digests to prove no rebuild.

These are pending live acceptance steps, not actions authorized by implementing the release tooling.

Runtime build source and runtime qualification source are distinct identities.
For a new bundle, follow [pre-adoption qualification](runtime-qualification.md#qualify-an-unselected-bundle-before-adoption): capture C's complete immutable candidate, qualify it at D without changing production admission, then review selection with the actual tested receipt and job IDs.
Version-2 bundle records carry D in `qualification.commit` and C in `source_commit`.
The release verifier binds both backend receipts to the exact Pi digest and inventory; C's product smoke of a different selected runtime is not evidence for C's unselected candidate.
Evidence artifacts must remain available; renew expired evidence for the same exact bundle rather than changing runtime contents merely to regain qualification.

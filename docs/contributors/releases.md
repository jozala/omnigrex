# Publish a product release

[Contributor testing](testing.md) · [Installation](../operators/installation.md)

Releases designate existing qualified builds; they do not deploy anything.
The accepted [release policy](../decisions/0053-manual-product-releases.md) is pre-1.0: stable means recommended for users, not production-ready or permanently backward-compatible.
Only Linux/amd64 is supported.

## Select an exact candidate

Choose an explicit **CI** run ID from a successful `push` to `main`, not a PR run, unrelated green check, workflow summary, mutable `sha-*` image tag, or current main tip.
All existing jobs must pass, including `check`, development qualification, both exact-digest deployment smoke jobs, and development Microsandbox smoke.
`publish` records the actual pushed digests and builds `deployment-payload-RUN-ATTEMPT`.
Both product smoke jobs verify and extract that payload and use its packaged Compose files with those captured digests.
Only afterward does `release-candidate` record job IDs and upload `release-candidate-RUN-ATTEMPT` with a machine-readable `candidate.json`, deployment payload, and checksums.
Main CI creates no version tag or GitHub Release.

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
The controller version remains its source commit.
`release.json` carries the designation, channel, breaking-change/upgrade declarations, commit, and payload checksum separately.
No release operation builds images, alters image labels, creates `latest`, changes `.omnigrex/environment.yaml`, or rebinds retained Agent Participants.
The Omnigrex-specific `development` image remains independently qualified and adopted by a separate repository change; other repositories supply their own environments, optionally derived from Pi.

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

## Failure and manual retry

Both entry points serialize through one concurrency group and never cancel an in-progress publication.
They use the Actions token with job-scoped `contents: write` and `actions: read`; no PAT or approval environment is required.
A duplicate version, draft, or existing tag fails rather than being overwritten or moved.
If upload/verification fails, inspect the incomplete draft and any associated tag manually.
Delete only the unpublished draft and its unused tag after confirming no published release or retained consumer depends on it, then rerun with the same inputs.
Never delete or retarget published versions to make a retry pass.
There is no automatic resume, cleanup, deployment, or package-visibility administration.

## Verification and first live acceptance

Local fixture tests cover qualification/provenance failures, versions, archive validation, no-checkout Compose, redesignation, and byte-preserving promotion with expired CI artifacts.
Run `mise exec -- go test -race ./.github/workflows` explicitly (hidden directories are excluded by `go test ./...`).
Use `mise run check` and `mise run test-integration`; KVM qualification remains in CI.

After merge and a new main candidate qualifies, **Mariusz or an explicitly authorized external agent** must:

1. Confirm both manual workflows appear and their actual token permissions work.
2. Intentionally publish `v0.1.0-alpha.1` from an explicitly selected qualified run.
3. Download and checksum the assets without a checkout, configure both overlays, and verify anonymous pulls of all product images.
   Existing post-publication smoke logs into GHCR and does not prove anonymous availability; package visibility is an administrator responsibility.
4. When intentionally approved, promote to `v0.1.0` and compare payload checksums and exact image digests to prove no rebuild.

These are pending live acceptance steps, not actions authorized by implementing the release tooling.

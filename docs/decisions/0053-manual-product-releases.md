---
status: accepted
date: 2026-10-30
decision-makers: [Mariusz]
---
# Designate manual pre-1.0 product releases from qualified builds

## Context
Omnigrex needs a small release process distinct from deployment and from adopting its own repository development environment.
Main CI already qualifies both Execution Backends and development images; rebuilding for a release would lose the identity of the tested artifacts.
Mariusz approved this policy in Work Item #160.

## Decision
Main CI produces finite-retention candidates only after the entire existing qualification succeeds, including both exact-digest product smokes and development checks.
A maintainer manually selects an exact source run and supplies a full pre-1.0 alpha version, release notes, breaking-change declaration, and upgrade prerequisites.
Alpha publication creates an immutable `v0.x.y-alpha.N` public prerelease without replacing the recommended stable release.
Stable promotion only removes that alpha suffix, creating a separate normal Release/tag at the same commit from retained alpha assets without rebuilding.
One coherent Linux/amd64 product set contains both controllers, generic Pi, and a tested downloadable deployment payload with exact digests, identity, evidence, and checksums.
The Omnigrex-specific development image stays independently qualified and adopted; product publication never changes repository environment configuration or retained Participant bindings.
Stable targets move forward on one current line; compatibility is declared by the maintainer against the previous stable, with breaking changes requiring a minor bump and compatible changes permitting patches.
A candidate may receive another alpha designation when its target changes; published versions are preserved.

## Rationale
Automatic releases on every merge would conflate qualification with a recommendation to users.
A second build would weaken artifact provenance and duplicate expensive qualification.
A custom release platform, signing program, approval environments, or comprehensive upgrade matrix would exceed this hobby project's needs.
GitHub controls, exact digests, checksums, and a draft/upload/verify/publish flow provide proportionate safeguards.

## Consequences
Stable means recommended, not production-ready or permanently backward-compatible.
Expired candidate evidence makes direct alpha publication ineligible, while retained alpha assets support later promotion independently of CI retention.
Historical upgrade unknowns and fresh-install requirements must be explicit; fresh-install smoke proves no general upgrade guarantee.
There is no deployment automation or automatic pruning of releases or protected images.
First live publication and anonymous image verification require separate human authorization after merge.

## Links
- [Work Item #160](https://github.com/jozala/omnigrex/issues/160)
- [Release procedure](../contributors/releases.md)
- [Aggregate qualification](0036-aggregate-parallel-ci-qualification.md)
- [Participant image pinning](0009-pin-participant-images-and-use-working-revision-tools.md)

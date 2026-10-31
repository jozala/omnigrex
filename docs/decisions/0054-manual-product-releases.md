---
status: accepted
date: 2026-10-30
decision-makers: [Mariusz]
---
# Designate manual pre-1.0 product releases from qualified builds

## Context
Omnigrex needs a small release process distinct from deployment and development-environment adoption.
Mariusz approved the release policy in Work Item #160 and its dedicated release App amendment on 2026-10-31.

## Decision
Main CI produces finite-retention candidates only after all existing qualification succeeds, including both exact-digest product smokes and development checks.
A maintainer manually selects an exact source run and supplies a full pre-1.0 alpha version, notes, breaking changes, and upgrade prerequisites.
Alpha publication creates an immutable `v0.x.y-alpha.N` prerelease without replacing recommended stable discovery.
Stable promotion removes only that suffix, creating a separate Release/tag at the same commit from retained alpha assets without rebuilding.
The Linux/amd64 product set contains both controllers, generic Pi, and a tested downloadable deployment payload with exact digests, identity, evidence, and checksums.
The development image remains independently adopted; product publication never changes repository environment configuration or retained Participant bindings.
Stable targets advance on one current line, with maintainer-declared compatibility judged against the previous stable: breaks require a minor bump, compatible changes permit patches.
Candidates may receive another alpha designation without altering earlier releases.

A dedicated release App installed only on this repository supplies a short-lived Contents/Workflows-write token exclusively for exact tag creation.
The workflow token retains other operations; no PAT or controller App credential is used.
Administrators provision the App and Actions variable/private-key secret separately; implementation uses Go and ordinary fixture tests.

## Rationale
Automatic releases would conflate qualification with recommendation; rebuilding would lose tested artifact identity.
A custom release platform, signing program, approval environments, or comprehensive upgrade matrix would exceed this project's needs.
The maintainer's probe demonstrated a workflow-token 403 tagging an unreferenced historical workflow-changing main ancestor.
A dedicated App with the advertised authority preserves automatic historical tagging with one-time setup instead of manually creating every version tag.

## Consequences
Stable means recommended, not production-ready or permanently backward-compatible.
Expired candidate evidence prevents direct alpha publication; retained alpha assets support promotion independently of Actions retention.
Upgrade unknowns and fresh-install requirements must be explicit.
There is no deployment automation or automatic pruning of releases or protected images.
App provisioning, actual historical tagging, first publication, and anonymous image verification remain separately authorized live acceptance.
App-created tags can trigger Actions, so retain manual-only releases and main-branch-only CI pushes.

## Links
- [Work Item #160](https://github.com/jozala/omnigrex/issues/160)
- [Approved authorization amendment](https://github.com/jozala/omnigrex/issues/160#issuecomment-6550576581)
- [Release procedure](../contributors/releases.md)
- [Aggregate qualification](0036-aggregate-parallel-ci-qualification.md)
- [Participant image pinning](0009-pin-participant-images-and-use-working-revision-tools.md)

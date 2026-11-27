# Microsandbox native image reclamation dependency

Tracking: [Work Item 218](https://github.com/jozala/omnigrex/issues/218).
Status: blocked on a qualified upstream contract; no upstream issue number has been verified.
Affected pin: `github.com/superradcompany/microsandbox/sdk/go v0.7.11` and the coordinated v0.7.11 native payload.

## Observed API boundary

Inspection of the pinned SDK's `image.go` found `Image.Get`, `List`, `Inspect`, `Remove(reference, force)` and `Prune`.
`List` returns the entire cache without pagination or a caller-supplied bound.
`Remove` documents sandbox-in-use rejection with `force=false`; `Prune` documents removing data not used by sandboxes.
`ImageDetail` exposes layers, and `ImagePruneReport` exposes counters and optional bytes.
These APIs exist, but their documented guarantees do not establish the required complete reference closure or native exclusion contract.
In particular, they do not establish protection of Omnigrex's offline retained Sessions, deployment/provisioning pins, index-to-platform aliases, snapshots, or concurrent external clients' pulls/materializations/executions.
The SDK sandbox inventory has pagination, but sandbox absence alone cannot authorize artifact removal.
No destructive SDK operation or native cache-file deletion is invoked by this implementation.

## Required upstream contract

Provide bounded inventory of manifests, aliases, shared layers, flattened/materialized artifacts, snapshots and in-flight users with stable identities and explicit incomplete provenance.
Provide a native transaction/reservation that revalidates explicit protected roots and complete reachability at apply and excludes every creator/user, including external clients, for the destructive operation.
Allow bounded, non-forced batches with durable operation identities and reconciliation of partial or unknown results across restart.
An Omnigrex-only mutex, an empty sandbox list, or a missing manifest is insufficient.
Any native upgrade must follow [the coordinated upgrade policy](../decisions/0043-coordinate-microsandbox-catalog-upgrades.md).

## Qualification required before enabling apply

Use disposable Linux/KVM fixtures and inspect CI for the exact implementation commit.
Cover active and offline retained Sessions; COLLECTING generations; deployment and explicit pins; shared layers; index/platform aliases; snapshots; concurrent pulls, materializations, executions and external clients; orphan flattened artifacts; incomplete inventory; no-force in-use rejection; interrupted batches; unknown removal effects; restart reconciliation; and apply-time root changes after dry-run.
Verify both preservation and actual allocated-block observations without conflating apparent sparse size, shared extents or open handles with physical reclamation.
Until then, `image-maintenance` reports bounded protection roots and unsupported native dimensions, and `--apply` refuses before resource access.
This fallback does not reclaim the orphan blobs reported in the original Work Item.

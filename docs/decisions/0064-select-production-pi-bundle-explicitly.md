---
status: accepted
date: 2026-11-10
decision-makers: [Mariusz]
---
# Select the production Pi bundle independently of orchestrator source

## Context
Coupling fresh Runtime Profiles to embedded checkout resources makes every orchestrator change an implicit runtime adoption decision.
Work Item #190 confirms that adoption coordination, not repeated CI build cost, is the problem.

## Decision
Check in one complete immutable production bundle selection per admitted guest platform, including resource inventory, exact golden digest, bootstrap requirements, and original qualification provenance.
Fresh Participants use this selection, including repository-image qualification; edits to an unselected runner candidate do not change it.
Initially production admission is Linux/amd64 only; reject fresh arm64 admission explicitly while preserving explicit arm64 local/candidate qualification and exact retained arm64 contracts.
Retained Participants resolve their original full canonical contracts from the ledger and validate their own inventories on every launch, independently of current selection.
A later-created Reviewer is fresh and can coexist with an older Developer.

Publish candidates together if convenient, but package the deliberately selected Pi digest without attributing it to the current orchestrator source/run.
Keep manual alpha designation, byte-preserving promotion, and both exact-pair product smokes.
Advance the selection only through a reviewed change after qualification and provisioning of the runtime and derived repository images.
Pause new work during coordinated selection/repository-pin cutover, run preflight, and retain protected historical images.
An image update alone is not a breaking release; declare actual contract incompatibilities separately from upgrade prerequisites.

## Rationale
A catalogue, automatic discovery, overlap admission, or capability negotiation adds compatibility policy not needed for one current bundle.
Runtime injection would violate self-contained image and immutable Participant guarantees.
Rebuilding equivalent resources or publishing a candidate is not evidence that adoption is necessary.

## Consequences
No repository runtime-selector field, Session migration, state disposal, or live publication is authorized.
The same-release golden assumptions in decisions 0026 and 0036 are replaced only for production-derived image qualification; candidate testing remains explicit.
Decisions 0009, 0012 and 0017 continue governing historical binding and resource verification.
Delivery and observed qualification remain tracked in the Work Item.

## Links
- [Work Item #190](https://github.com/jozala/omnigrex/issues/190)
- [Platform scope answer A1](https://github.com/jozala/omnigrex/issues/190#issuecomment-6763624864)
- [Operator cutover](../operators/upgrades.md#runtime-profile-image-change)
- [Manual releases](0054-manual-product-releases.md)

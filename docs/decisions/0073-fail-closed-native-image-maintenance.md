---
status: accepted
date: 2026-11-27
decision-makers: [jozala]
---
# Fail closed when native image reclamation cannot be qualified

## Context

[Work Item 218](https://github.com/jozala/omnigrex/issues/218) reports Participant-local leftovers and separate native flattened-image artifacts remaining after manifest removal.
Removing Participant storage and reclaiming installation-wide native artifacts require different ownership and concurrency authorities.
A missing manifest or live sandbox is not proof that an artifact has no protected or concurrent users.

## Decision

In [A1](https://github.com/jozala/omnigrex/issues/218#issuecomment-7206880182), jozala accepted the recommended fail-closed native capability gate and explicitly excluded historical already-collected Participant cleanup.
Complete future Participant collections independently of native image maintenance.
Native deletion requires qualified native reachability and creator/user exclusion, including external clients.
If those guarantees are unavailable, deliver bounded protection inventory, explicit unsupported/incomplete reporting, apply refusal and a tracked dependency rather than destructive pruning.

## Rationale

Blanket native pruning or deleting flattened files based on absent manifests risks retained Sessions, shared layers, aliases and in-flight users.
An application-only lock cannot coordinate external clients.
Making Participant completion wait on unsupported installation-wide GC would conflate independent lifecycles.
An automatic historical sweep would contradict the owner's explicit scope decision.

## Consequences

Reported orphan blobs may remain unreclaimed until the upstream safety contract is qualified.
Operators retain responsibility for historical leftovers and must supply explicit provisioning/pinning roots to inventory; incomplete roots never imply deletion permission.
No native-stack upgrade, retention shortening, deadline rewrite or database-history deletion is authorized.
Delivery status and required qualification are tracked in the [implementation design](../plans/2026-11-27-participant-storage.md) and [dependency record](../dependencies/microsandbox-safe-image-gc.md); acceptance is not deployment evidence.

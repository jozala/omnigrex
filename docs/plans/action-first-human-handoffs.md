# Action-first Human Handoffs

Implementation notes for [Work Item 235](https://github.com/jozala/omnigrex/issues/235), accepted pair P1.

## Contract and boundaries

`report_blocked.context` is an optional, concrete six-text-field value: explanation, intervention, completed work, observations, uncertainty and evidence.
Each field has a distinct presentation placement; only evidence is expandable.
The existing diagnostic sanitizers stay unchanged.
Context passes through exact terminal evidence validation, credential redaction, canonical settlement, the reducer's passive action, and durable publication job JSON.
No schema migration or new reporting subsystem is needed.
Recorded operations without context still reconstruct their original result shape.

Normal publication reads the current revision's exact reason-matched non-safety payload.
Tracking-only synchronization carries the immediately preceding payload's context and authoritative source observation head, not arbitrary historical prose.
Newly published heads come from settlement publication proof; otherwise the source is the Turn's expected head.
This never drives transitions, authorization, qualification or automatic recovery.
See [decision 0071](../decisions/0071-separate-handoff-communication-from-authority.md).

## Rendering and verification

The renderer owns the action-first template, artifact association, continuation and expandable HTML.
A deliberately limited Markdown path preserves paragraphs, lists, emphasis, inline code and HTTP(S) links while neutralizing raw HTML, images, reference links, fences and publication-marker injection.
Input is bounded by JSON-encoded bytes, leaving room for escaping, diagnostics, boilerplate and markers.
Legacy diagnostics use an explicitly labeled bounded excerpt when necessary.

Focused regressions cover tool intake/recovery and exact evidence, every context field's redaction, visible actions/risks, safety fallback and malformed formatting.
The Store convergence fixture exercises settlement, durable job read-back, tracking/head movement, Issue/current-PR publication and ambiguous remote success through the existing workers.
These fixtures establish mechanisms, not real-model communication quality or CI qualification of reported work.

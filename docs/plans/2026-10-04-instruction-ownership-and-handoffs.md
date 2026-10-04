# Instruction ownership, deterministic Git identity, and visible handoffs

## Approved scope

Implement the decisions agreed on 2026-10-04 after checkpoint commit `3b2be2d`, on `feat/agent-instructions-and-handoffs`.
Repository Agent Profiles should describe personality, communication preferences, repository expectations, and skill selection.
Omnigrex should provide its own platform and Stage instructions, allow operator-authored common and per-Role guidance, configure a usable Git identity, and make every new review handoff visible before continuing.
Add a general decision log and a native repository skill explaining when and how to maintain it.

## Instruction sources and authority

1. Platform instructions ship with Omnigrex and explain MCP-only GitHub access, truthful verification, terminal-intent completion, and escalation.
2. The current Workflow Definition supplies Stage guidance, including purpose-specific initial implementation, requested changes, review, and recovery instructions.
3. Operator Role instructions take precedence over operator common guidance; both constrain repository preferences without changing code-enforced permissions or Workflow transitions.
4. Repository Profiles select skills and describe personality and communication style.
5. Repository skills supply the engineering procedures and obey the current Stage contract.

Compose these sources immediately before launching an Agent Turn, outside the runtime-specific adapter.
Use platform and Workflow guidance from the running binary and operator files loaded once during startup.
Never persist a composed instruction snapshot as a future source of execution instructions.
Restarting Omnigrex makes the new deployment guidance apply to subsequent launches, including continued Agent Sessions and interrupted-work recovery.
An already executing Runtime Process is not hot-reconfigured.
Existing repository Profile provenance and snapshots remain distinct from these deployment instructions.

## Operator files

Support an optional `OMNIGREX_AGENT_INSTRUCTIONS_DIR` containing `common.md` and `roles/<ROLE>.md`.
Read the directory only in the orchestrator and mount it read-only through an optional Compose override.
No directory configured means no operator additions; absent common or Role files are allowed.
A configured missing directory, unreadable files, malformed UTF-8, oversized content, or an unknown Role file must fail startup with an actionable diagnostic that does not print instruction contents.
Use the deployment Role Policy Catalog to validate Role names rather than repository Profile identities.
Bound both raw file size and the combined JSON-encoded operator contribution, then validate the fully composed runtime configuration in preflight and launch against the environment-string limit.
Include the same validation in preflight.
Do not add file watching, a templating language, model selection overrides, or instruction files inside the Runtime Process workspace.

## Stage guidance and Profile cleanup

Associate guidance with the code-defined Stage, with optional per-Turn-purpose additions, rather than hardcoding the next Role into every Developer Profile.
Preserve this metadata when copying and validating Workflow Definitions.
The built-in implementation Stage explains clean committed publication, opening a PR only when needed, summarizing revisions, and requesting the next Stage through `request_review`.
The built-in review Stage explains evidence-based approval, previous findings, current-head verification, and native inline findings through `submit_review.comments` where the diff supports a valid location.
Keep read-only tracked-file treatment and disposable Reviewer scratch-space guidance in code-owned Role/runtime instructions.
Retain existing targeted recovery guidance from the Turn envelope.
Move mergeability guidance to the appropriate Stage; deterministic enforcement is a separate follow-up.

Profiles should use direct, calm, factual prose, lead with outcomes, expand for important tradeoffs and uncertainty, and avoid repetitive implementation diaries.
Developer PR descriptions explain what changed, why, and verification; later summaries focus on changes since the last review.
Reviewer findings focus on condition, consequence, and required correction; straightforward approvals remain one or two sentences.
Update skills to refer to current Stage completion criteria, Profile communication style, and orchestrator escalation guidance.
Keep native skill invocation unchanged and do not add unsupported invocation front-matter fields.

## Deterministic Git identity

Set fixed repository-local `user.name` and `user.email` during staged workspace preparation before promotion and agent launch.
Use `Omnigrex` and `agent@omnigrex.invalid` for now, for every prepared workspace.
Do not change the operator's global Git configuration, require GitHub identity lookup, or rewrite existing commits.
Test a normal agent commit and merge commit without the agent setting an identity.

## Durable handoff publication and retrieval

Reuse `request_review` and its mutation reservation as the durable handoff request.
For new requests, validate the scoped PR/head, publish a concise signed PR comment containing the head and supplied summary, and return successful terminal intent only after publication is confirmed.
Use the existing reservation UUID as the stable GitHub operation marker and retain the reserved signature across retries.
The comment is an external GitHub effect, so newly reserved handoff mutations must be distinguishable from historical internal-only requests in recovery.
Read back the exact marker and expected body after uncertain responses; never infer publication from the existence of the PR alone.
Reuse existing bounded mutation recovery and Human Handoff behavior when publication cannot be established.
The normal Workflow reducer must see no successful new handoff intent before comment publication, so the next Stage cannot be admitted early.
Retain compatibility for already completed historical internal-only handoffs rather than inventing an unperformed old comment effect.

Expose a read-only `get_handoff` MCP tool for the latest successful review handoff belonging to the scoped Workflow and PR head.
Return its source head, summary, and durable identity, and explicitly return no handoff when none matches the scoped head.
Never return another Workflow's summary or silently treat older-head evidence as current.
Provide this tool to both built-in Roles and tell the review Stage to retrieve it as a Developer claim to verify.
The original summary remains in the ledger; agents need not repeat it in separate comments or reopen an existing PR after every revision.

## Decision log

Use `docs/decisions/README.md` as an index and create one numbered, short Markdown record per consequential decision, including non-architectural decisions.
Keep existing `docs/adr/` records at their current paths and link them from the index; do not duplicate their decisions.
Record context, outcome, rationale/alternatives, consequences, status/date/decision makers, and related work.
Accepted describes a decision, not implementation progress; replacement decisions supersede earlier records instead of rewriting history.
Keep the template and procedural guidance in `.agents/skills/omnigrex-decision-records/SKILL.md`, with a short trigger in `AGENTS.md`.
The Reviewer may inspect records and identify omissions but does not gain editing authority.

## Implementation and verification sequence

1. Add the decision log, initial agreed records, and the decision-record skill.
2. Implement and test the deployment instruction loader/composer and Stage metadata, then wire startup, launch, preflight, and deployment documentation.
3. Add the workspace identity setup and focused commit/merge regression tests.
4. Implement handoff publication, exact-effect reconciliation, scoped Store retrieval, MCP registration, and terminal evidence compatibility.
5. Add regressions for first PR and subsequent Review Cycles, rejected/uncertain publication, lost responses, replay, current-head scope, and changed instruction content after restart with Session Continuation.
6. Revise Profiles, skills, and operator guidance to match the actual code contracts.
7. Run focused suites, `mise run check`, and `mise run test-integration`; run destructive Compose tests only with explicit permission to recreate their resources.
8. Review against repository standards and these requirements and record any remaining deployment-only follow-ups.

## Deferred work

- Deterministic mergeability enforcement, beyond Stage instructions.
- The skill-change human-approval gate in [issue #43](https://github.com/jozala/omnigrex/issues/43).
- Pi migration, live instruction-file reload, operator-configurable Git identities, and a general handoff UI.

## Implementation and verification record

Implemented locally on `feat/agent-instructions-and-handoffs`, after checkpoint commit `3b2be2d`.
The instruction loader/composer is in `internal/agentinstructions/`, Stage guidance is embedded under `internal/workflow/instructions/`, and launch and preflight use the current deployment sources.
Operator files are loaded once at startup and no effective instruction snapshot was added to the Store.
Workspace preparation supplies the fixed Git identity before promotion.
New `request_review` mutations publish signed comments before successful completion, and `get_handoff` reads matching durable summaries through the scoped Store query.
The decision log and native decision-record skill are linked from the repository instructions.

Verification passed:

- `mise run check`, including formatting, static analysis, dead-code checks, race-enabled unit tests, and Compose configuration validation.
- `mise run test-integration`, covering the pinned ACP runtime, Docker behavior, Store, doctor, and deployment tests.
- Focused tests for operator loading and Role scope, current instructions after Session Continuation, commit/merge identity, handoff publication blocking, lost-response recovery, duplicate prevention, and cross-Workflow/PR/head read isolation.
- Skill front-matter validation for `omnigrex-decision-records`.

The standards review found one sentence-per-line issue in the approval example, which was corrected.
The spec review found an environment-string size risk from combined operator files; combined JSON-encoded and full runtime configuration limits now have regression coverage, and the fix passed focused re-review.
No review findings remain unresolved.
The Docker-backed tests used local ARM64 images and a controlled provider; no production deployment, live model evaluation, or destructive full-stack Compose test was performed.

# Repository-local skills and focused Agent Profiles

## Status and goal

Prepared on 2026-10-04 from the discussion about improving Developer implementation and Reviewer evaluation quality.
The implementation is complete locally, with automated verification recorded below; deployment and model-quality evaluation are separate follow-ups.

Enable both Roles to discover and load native skills from the current working checkout, then move the detailed implementation and review procedures into two concise repository-owned skills.
Keep the Agent Profiles authoritative for Role responsibilities, approval criteria, permissions, and Omnigrex publication and Human Handoff rules.
Improve requirement coverage, behavior-focused testing, evidence-based completion claims, and actionable review findings without introducing sub-agents.

The human explicitly selected checkout-local skills instead of a separately prepared and pinned default-branch skill bundle.
A separate future enhancement will require human approval when a Change Proposal changes skills.
Native skill support does not depend on that future enhancement, and its initial rollout must not claim the approval gate already exists.

## Current behavior and implementation anchors

- `.omnigrex/team/developer.md` and `.omnigrex/team/reviewer.md` contain Role instructions, model configuration, and explicit local-tool permissions.
- Neither Profile currently grants `skill`; omitted permissions default to deny in `internal/agentprofile/profile.go`.
- The Profile parser already recognizes `skill`, and `internal/runtime/opencode/render.go` already translates an allowed local tool into an OpenCode permission request handled by Omnigrex.
- `internal/runtime/opencode/permission.go` already includes `skill` in the ACP permission decision for local tools classified as `other`.
- Reviewer hardening sets `OPENCODE_DISABLE_EXTERNAL_SKILLS=true`, `OPENCODE_DISABLE_PROJECT_CONFIG=true`, and related flags.
- In the pinned OpenCode 1.18.29 source, external skill discovery includes repository and home-directory `.agents/skills/` and `.claude/skills/` trees.
- Disabling project configuration also excludes the repository `.opencode/` directory from automatic configuration-directory discovery.
- OpenCode's explicit `skills.paths` discovery is processed independently of those two discovery restrictions.
- `internal/runtime/acp/opencode_integration_test.go` contains `TestOpenCodeReviewerIsolationBoundary`, whose feature-branch fixture currently includes disallowed skills alongside other configuration.
- `internal/role/policy.go` remains the authority for Role capabilities, credential authorities, Reviewer workspace disposal, and runtime hardening.
- `internal/agentturn/runtime.go` supplies the prepared checkout and rendered configuration to a fresh Runtime Process while supporting Agent Session continuation.

The source inspection establishes a feasible mechanism, not an end-to-end compatibility result.
Prove the mechanism with the pinned image before relying on it in production.
Pinned source references: [skill discovery and availability](https://github.com/anomalyco/opencode/blob/v1.18.29/packages/opencode/src/skill/index.ts), [configuration-directory selection](https://github.com/anomalyco/opencode/blob/v1.18.29/packages/opencode/src/config/paths.ts), and [native skill tool](https://github.com/anomalyco/opencode/blob/v1.18.29/packages/opencode/src/tool/skill.ts).

## Decisions

### 1. Repository layout and native loading

Use the following layout:

```text
.omnigrex/team/
  developer.md
  reviewer.md
.agents/skills/
  omnigrex-implementation/
    SKILL.md
  omnigrex-code-review/
    SKILL.md
```

Each skill uses ordinary `SKILL.md` front matter with a unique `name` matching its directory and a concise `description` explaining when to load it.
Use the `omnigrex-` prefix to reduce collisions with other installed skills.
Keep any later examples or supporting references inside their owning skill directory and load them only under an explicit condition.
Do not place helper Markdown files directly under `.omnigrex/team/`, where every direct Markdown file is parsed as an Agent Profile.

Add `skill: allow` to both Profiles and register `/workspace/.agents/skills` through Omnigrex's generated OpenCode configuration:

```json
{
  "skills": {
    "paths": ["/workspace/.agents/skills"]
  }
}
```

Emit the repository skill path when the effective Profile permits `skill`, using the existing workspace contract rather than introducing a configurable arbitrary host path.
Profiles that do not enable skills should retain their existing behavior.
No new Agent Profile front-matter field is needed.
Do not add a skill download URL, a new MCP service, a repository `opencode.json`, or a separate skill installation step.

Retain Reviewer project-configuration and external-discovery restrictions and use the explicit path as the narrow exception.
This path adds a known source; it is not a claim that every other source available under the Developer's existing configuration is excluded.
Skill loading changes available guidance, not Role permissions or MCP authority.
Keep `task` denied so that enabling `skill` does not enable delegation.

### 2. Revision and lifetime

Load skills from the checkout actually prepared for the Agent Turn, including Change Proposal skill edits when that checkout contains them.
Do not add a default-branch skill export, extra mount, durable skill snapshot, or new skill-version database schema in this work.
Existing Agent Profile discovery continues to use its current default-branch provenance contract.

Do not assume OpenCode hot-reloads an edited skill during an active Runtime Process.
Verify that a subsequent fresh Runtime Process discovers the new checkout content, including when it continues an existing Agent Session.
The profile should instruct the agent to load its Role skill at the start of every Agent Turn so an earlier conversational copy is not silently treated as the current procedure.
If a required skill cannot be loaded, use `report_blocked` with the missing skill name and actionable details rather than silently falling back to an incomplete procedure.
Repositories and Profiles that do not request these skills do not acquire that requirement.

### 3. Instruction ownership and length

Profiles own enduring Role obligations and the conditions under which work can be handed off.
Skills own the step-by-step method for meeting those obligations.
Repository instructions, `CONTEXT.md`, and applicable ADRs remain the source of repository-specific conventions and test commands.
Reference those sources rather than copying a repository map or command catalogue into each skill.

Aim for roughly 250-450 words per initial skill and keep each Profile short enough to read as a compact Role contract.
Treat these as editing targets, not runtime limits or reasons to remove necessary publication rules.
Use direct instructions with observable completion criteria rather than repeated emphatic prohibitions or generic requests to be thorough.
Keep each full sentence on one physical Markdown line.

## Agent Profile changes

### Developer

Retain the assigned Work Item, MCP-only GitHub access, repository instruction compliance, and blocker-reporting obligations.
Add an explicit requirement to load `omnigrex-implementation` before implementation or revision work on each Agent Turn.
State that all work is performed directly with available tools and that sub-agents are unavailable.

The Profile's completion contract should require a complete solution to the Work Item and verification evidence for the actual changes being handed off.
Keep the existing mergeability check, conflict-resolution process, prohibition on rewriting published commits, clean committed workspace requirement, local Git identity fallback, and publication-history warning.
Keep publishing, Pull Request creation or update, and Reviewer handoff through the appropriate Omnigrex MCP tools.
Keep compact verification evidence and limitations in the Pull Request or `request_review` summary, rather than adding routine progress comments for each test.

### Reviewer

Add an explicit requirement to load `omnigrex-code-review` before evaluating the Change Proposal on each Agent Turn.
State that the Reviewer performs its own sequential passes and cannot delegate.
Make repository-instruction and applicable-ADR compliance explicit.

Keep MCP-only GitHub access, inspection of collaboration state and current-head check results, read-only treatment of tracked files, disposable verification scratch files, and read-only mergeability checking.
Keep the requirement to re-evaluate substantive earlier findings, including human findings and findings on earlier heads.
Clarify that each earlier finding must be resolved, shown by evidence not to apply, or retained as an unresolved finding; a newer head or dismissed thread alone is not evidence of resolution.
Escalate a substantive unresolved requirements disagreement through Human Handoff rather than silently overriding it.

The Profile's approval contract should require Work Item satisfaction, no supported unresolved Blocking Findings, and sufficient evidence for a defensible verdict.
Optional cleanup and personal style preferences are Non-blocking Findings and must not force a changes-requested outcome.
Keep the existing one- or two-sentence approval format, including omission of implementation recaps, verification logs, resolved findings, and optional cleanup suggestions.
Use `report_blocked` when missing information or required verification prevents evaluation; do not manufacture a code defect to explain an environment limitation.

## Skill content

### `omnigrex-implementation`

Organize the skill into four short sections with the following requirements.

1. **Understand:** Read the Issue, relevant comments, and existing code and tests; identify required observable behaviors, affected interfaces, and verification methods before editing.
   Resolve routine implementation choices from repository patterns and escalate only ambiguity that materially changes required behavior or architecture.
2. **Implement:** Build a simple, complete, maintainable solution in small increments; reuse established patterns and keep refactoring tied to the Work Item.
   For a bug, reproduce the symptom where feasible and trace its cause before fixing it.
   For behavior changes, prefer one failing behavioral test followed by the implementation, checking that the failure is for the intended reason.
   Choose established test boundaries without requiring a new human approval for every test.
   Derive expected results from requirements or independently worked examples, not a copy of the implementation.
   Cover the relevant failure paths and boundaries without creating tautological tests for documentation-only or trivial changes.
3. **Verify:** Run focused checks while iterating and the repository-required checks after the final relevant changes.
   Investigate failures rather than disabling checks or weakening assertions merely to get a pass.
   Distinguish passed, failed, skipped, and unavailable checks and use current-head CI evidence when it covers a local gap.
   Inspect the full diff for missing requirements, unintended changes, and unnecessary complexity before handing off.
   Evidence must apply to the final code; rerun checks after changes that invalidate it, not before every status message.
4. **Handle feedback:** Validate each substantive finding against current code and requirements, fix supported defects with regression coverage where practical, and explain disagreements with concrete evidence.
   Use focused hypotheses when debugging instead of accumulating unrelated speculative fixes.

Testing must account for Omnigrex's actual environment.
Runtime Processes lack the Docker socket, and the Reviewer toolchain is selected from the default branch.
Unavailable Docker-backed checks or a toolchain mismatch are verification limitations, not automatically defects in the Change Proposal.
Missing required evidence still needs resolution through the existing Workflow or Human Handoff.

### `omnigrex-code-review`

Organize the skill into scope, two passes, validation, and verdict.

1. **Scope:** Confirm the checkout matches the scoped Pull Request head and inspect the complete Change Proposal relative to the merge-base with its base branch.
   Derive expectations from the Issue, relevant human clarification, repository instructions, and applicable ADRs.
   Treat the Developer's summary as a claim to verify, not evidence of correctness.
2. **Requirements pass:** Account for every required behavior and identify missing, partial, or incorrect implementation and unjustified scope expansion.
3. **Correctness and maintainability pass:** Trace changed behavior through relevant callers and dependencies.
   Check applicable failure paths, compatibility, security boundaries, concurrency, retries, and test effectiveness.
   Apply repository conventions and accepted architecture, and distinguish material design problems from personal preferences.
   Perform both passes directly; sequential passes organize attention but do not provide independent sub-agent contexts.
4. **Validate:** Investigate suspected findings using a focused check or a concrete execution path, identify the triggering condition and consequence, and verify the affected code.
   Separate newly introduced defects and unmet Work Item requirements from unrelated pre-existing problems.
   Inspect the entire scope before submitting, consolidate duplicates, and prioritize supported findings by impact.
5. **Verdict:** For each Blocking Finding, give the relevant file and location, the condition that exposes the problem, its consequence, supporting code or requirement, and the correction needed.
   Use native inline review comments where a valid changed-line location exists and the review body for cross-cutting or missing-behavior findings.
   One finding should describe one actionable problem without prescribing an unnecessary rewrite.
   Apply the Profile's approval and Human Handoff rules and preserve concise approval output.

Do not transplant a long generic code-smell catalogue, arbitrary numeric confidence threshold, mandatory praise section, or delegation instruction.
Maintainability can justify a Blocking Finding when its concrete consequence is explained; review is not limited to bugs that already have a runnable reproducer.

## Implementation sequence and verification

### Phase 1: Prove native loading in the pinned runtime

Extend the controlled-provider ACP integration fixture to expose a uniquely named repository skill and issue an actual `skill` tool call.
Run the fixture with both Developer and Reviewer effective permissions and with the Reviewer's existing hardening flags still set.
Prove that the provider sees the skill description and receives the correct body only through loading, and that an unenabled Profile cannot use the tool.
Prefer the existing Runtime Process and fake-provider seam over a separate ad hoc runtime harness.

Completion criterion: the pinned image demonstrates discovery, native loading, permission enforcement, and continued denial of sub-agent tools.

### Phase 2: Wire the generated configuration

Add the conditional `skills.paths` field to the renderer and test its JSON output for both Roles and a Profile without skill access.
Exercise the existing ACP permission mapping for the skill request and verify unrelated denied tools remain denied.
Cover effective configuration through the existing Agent Turn launcher tests, not just a serialized JSON snapshot.
Confirm missing skill directories are compatible with repositories that do not use this feature.

Likely implementation files are `internal/runtime/opencode/render.go`, its tests, and focused permission and launcher tests under `internal/runtime/opencode/` and `internal/agentturn/`.
The existing Profile schema and permission handling should be sufficient; change them only if the runtime proof identifies a real gap.

Completion criterion: the actual launch contract contains the intended repository skill source and preserves Role authorization.

### Phase 3: Add skills and update Profiles

Write the two skills using the content requirements above, then revise the Profiles to refer to them explicitly.
Review the combined Profile, skill, and repository instructions for conflicting requirements, duplicated procedures, unavailable tools, and implicit human-interaction assumptions.
Keep the existing model, variant, and step-budget settings so later quality comparisons isolate the instruction change.
Do not call local personal skills or assume upstream skill repositories are installed in the Runtime Process.

Completion criterion: both Roles have one explicit native skill entry point, complete operational contracts, and no sub-agent requirements.

### Phase 4: Update compatibility expectations and documentation

Separate the authorized repository-skill marker from the existing prohibited feature-branch configuration marker in `TestOpenCodeReviewerIsolationBoundary`.
Assert that skills in the registered directory are available while unrelated project agents, MCP configuration, and permission overrides remain subject to the existing boundary tests.
Do not remove the broad isolation assertions or claim this work repairs previously documented plugin or nested-instruction compatibility limitations.

Cover missing or malformed required skills, a denied skill tool call, and a new Runtime Process using changed skill content while continuing a retained Agent Session.
Check that test-created scratch files do not become production workspace changes and that Reviewer tracked-file rules remain intact.

Record the deliberate checkout-skill exception in a focused ADR and update current implementation and operator documentation where it would otherwise claim all feature-branch skills are excluded.
Link operational procedures to `docs/operator-guide.md` rather than duplicating deployment or secret instructions.
Do not modify the accepted `docs/plans/first-iteration.md` specification.

Completion criterion: documented boundaries and pinned-runtime tests agree on which skill source is now permitted.

### Phase 5: Run the repository gates and controlled rehearsal

Use the exact development commands from `README.md` and `mise.toml`.

```sh
mise install
mise exec -- go test ./internal/agentprofile ./internal/runtime/opencode ./internal/agentturn
mise run check
mise run test-integration
```

Run focused ACP integration cases during iteration; the full Docker-backed gate is required because runtime behavior changes.
Run `mise run test-compose` only when the stable local test networks and volumes may be removed, as required by the repository instructions.
Record any unavailable gate as a limitation rather than a pass.

After the updated Profiles and skills are present on the default branch and the runtime change is deployed, use a controlled Work Item to verify the Developer loads its skill, publishes through MCP, and hands off to a Reviewer that loads its own skill and produces the expected concise verdict.
Include a subsequent Turn with changed skill content to confirm the selected checkout is the source and that no default-branch skill pinning has been introduced.
Treat real-provider behavior as a rehearsal in addition to, not a replacement for, deterministic compatibility tests.

## Separate future enhancement: approval for skill changes

Follow-up: [Require human approval before continuing with changed repository skills (#43)](https://github.com/jozala/omnigrex/issues/43), recorded as an enhancement and postponed for later implementation.
Implement an orchestrator-enforced human approval gate, with skill changes as its first use case.
The gate must detect changes independently of agents and authorship, report the affected files and revision, and pause autonomous continuation until an authorized human explicitly approves.
Detection must cover the whole registered skill tree, including supporting references and scripts, rather than only `SKILL.md` files.
Approval must be scoped to the Work Item, Change Proposal, policy, and protected-change identity so it cannot silently authorize later skill edits.
Record evaluated revisions and recheck each new head; a code-only commit may reuse approval only when the protected-change identity is proven unchanged.

Reuse existing Human Handoff, fencing, durable action, and recovery mechanisms rather than relying on a prompt instruction or an agent-issued review.
Design the persisted decision so other orchestrator-owned reasons can require approval later, but implement only the skill-change detector initially.
Keep the future gate runtime-independent.
Specify publication and in-flight synchronization behavior carefully: exposing a candidate for human inspection must not also authorize another autonomous Turn or a ready-for-human verdict.
The gate is a separate feature, not a skill-loading prerequisite or a promise of instant interception of every local filesystem edit.

## Interaction with the planned Pi runtime

[Replace OpenCode with Pi RPC as the sole agent runtime](https://github.com/jozala/omnigrex/issues/33) is currently postponed and proposes disabling project skills.
The checkout-local skill decision in this plan should be reconciled with that future runtime's resource-discovery contract when its implementation resumes.
Keep the `SKILL.md` content runtime-portable and isolate OpenCode-specific discovery wiring in the current adapter.
Do not expand this work into the Pi migration.

## Evidence and quality evaluation

The procedures are informed by the local implementation and review skills, [Matt Pocock's TDD skill](https://github.com/mattpocock/skills/blob/main/skills/engineering/tdd/SKILL.md), [writing-for-agents guidance](https://github.com/mattpocock/skills/blob/main/skills/productivity/writing-for-agents/SKILL.md), [Superpowers verification](https://github.com/obra/superpowers/blob/main/skills/verification-before-completion/SKILL.md), [review reception](https://github.com/obra/superpowers/blob/main/skills/receiving-code-review/SKILL.md), [OpenAI's review rubric](https://github.com/openai/codex/blob/main/codex-rs/prompts/templates/review/rubric.md), and [Google's review standard](https://google.github.io/eng-practices/review/reviewer/standard.html).
Adapt the useful procedures rather than copying platform-specific commands, delegation, or human-approval assumptions verbatim.
Preserve attribution and applicable license notices if implementation copies substantial upstream text.

[Evaluating AGENTS.md, revision 3](https://arxiv.org/abs/2602.11988v3) studied 300 SWE-bench tasks and 138 additional tasks and found no significant overall success improvement from context files, alongside increased cost.
[SkillsBench, revision 4](https://arxiv.org/abs/2602.12670v4) reports an average pass-rate improvement from 33.9% to 50.5% across 87 tasks and 18 model-harness configurations with curated skills.
The latter includes procedural packages that can contain executable resources and has task-selection and domain limits; neither study proves these exact instructions improve Omnigrex or that native skill packaging is inherently better than equivalent inline text.

After functional qualification, optionally run a paired quality pilot using old and new instructions on the same historical Work Items and review snapshots, with the same models, budgets, and environments.
Include correct Change Proposals, known defects, missing requirements, and environment-only failures; withhold historical solutions and later review answers from the agents.
Repeat runs and use independent tests and human adjudication to compare correctness, missed defects, false Blocking Findings, Review Cycles, and execution cost.
Twenty to thirty cases are a useful pilot, not a statistically conclusive claim of improvement.
Use observed failure modes to refine or remove instructions rather than accumulating speculative checklist items.

## Completion checklist

- [x] Both Roles discover and load native repository skills in the pinned Runtime Process.
- [x] Skills come from the current checkout and refresh on a subsequent Runtime Process with Agent Session continuation.
- [x] Profiles without skill permission retain existing behavior, and task/delegation permission remains denied.
- [x] Reviewer isolation tests distinguish the allowed skill directory from other project configuration.
- [x] Profiles retain publication, mergeability, prior-finding, approval-output, and Human Handoff requirements.
- [x] Skill procedures are concise, evidence-oriented, and executable without sub-agents or mandatory routine human questions.
- [x] Required local and Docker-backed checks pass.
- [x] Current architecture and operator documentation describe the checkout-skill exception accurately.
- [x] The separate human-approval enhancement is linked and is not represented as already implemented.

## Execution record: 2026-10-04

Implemented conditional native skill-path rendering, both repository skills, updated Agent Profiles, and the checkout-local decision in [ADR 0012](../adr/0012-load-repository-skills-from-the-current-checkout.md).
The first focused renderer test failed for the intended reason: allowed Profiles had no repository skill source before the change.
The pinned OpenCode 1.18.29 image then passed controlled-provider integration tests for both Roles, fresh-process skill updates with retained Agent Sessions, omitted skill permission, a missing directory, invalid skill front matter, and ACP rejection.
The rejection test checks the failed tool event because OpenCode ends the Turn after a rejected skill load without emitting a final assistant message.

Verification completed successfully:

- `mise install`
- `mise exec -- go test ./internal/agentprofile ./internal/runtime/opencode ./internal/agentturn`
- Focused race-enabled ACP integration tests for repository skill loading and rejection.
- `mise run check`, including formatting, static analysis, dead-code checks, race-enabled unit tests, and Compose configuration validation.
- `mise run test-integration`, including the ACP, Docker, Store, doctor, and deployment suites.
- Independent static standards and plan-compliance reviews, with no findings on either axis.

The integration tests exercised the local ARM64 images and controlled provider; they establish runtime behavior, not an improvement in real-model implementation or review quality.
The deliberately destructive `mise run test-compose` was not run.
The deployed Work Item rehearsal and optional paired model-quality pilot remain follow-ups; no deployment or live Workflow was initiated for this implementation.

# Native context compaction

[Operator guide](README.md) · [Agent policy](agent-policy.md)

Compaction-enabled Pi runtime images use Pi 1.4.0's native automatic context compaction and bounded overflow recovery.
Pi estimates context, generates a checkpoint summary, and projects that summary plus retained recent messages into subsequent model requests.
Successful recovery continues the authorized assignment without a new Session, a workspace reset, or human permission.
Compaction is maintenance within the same Agent Turn, not a completion receipt or a new retry allowance.
Ordinary automatic provider retries remain disabled; controller retry and Human Handoff policy are unchanged.

## Thresholds and capacity

The controlled in-memory settings enable compaction without overriding Pi's defaults: 16,384 reserved tokens and 20,000 recent tokens to retain in Pi 1.4.0.
Pi decides when to compact using these settings and the selected model's pinned catalog capacity metadata.
Repository settings cannot supply thresholds, capacity overrides, extensions, or replacement summary logic.
There is no new deployment setting or recovery-budget knob.
Summary requests use the selected provider and consume provider tokens even though they are not conversational steps.

Explicit context/input-overflow errors retain only a fixed safe marker through the pre-persistence sanitization boundary so Pi can compact and retry natively.
Ambiguous token-limit messages and account quota, rate-limit, or authentication failures are not promoted to overflow.
Pi makes at most one compact-and-retry attempt for an overflowing response before reporting unsuccessful recovery.
Compaction cannot guarantee indefinite execution, recovery from every oversized tool result, or recovery when a single request cannot be reduced sufficiently.
It does not change existing execution deadlines, mutation fences, publication safeguards, or tool permissions.
Where conversational step budgets are available, summary maintenance does not replenish them; dedicated combined qualification is tracked in [#130](https://github.com/jozala/omnigrex/issues/130).

## State, diagnostics, and failure handling

Summary bodies and transcripts remain in protected Session storage with the same identity and retained append-order history.
History Replay still reports prior entries rather than only the compacted model-visible projection.
Reopening uses current-deployment instructions; summaries and older instruction records are not a new source of authority.
Apply the existing [retention](retention.md), [backup](backup.md), and [restore](restore.md) procedures to this protected state.

Safe runtime activity identifies `compaction_start` and `compaction_end`.
A failed end reports `compaction_failed`, without summary bodies, prompts, tool contents, secrets, or provider error text.
An unresolved compaction failure at settlement uses existing bounded provider-failure handling rather than reporting completion; cancellation is not completion either.
If native pre-prompt compaction fails but Pi subsequently completes a conversational request successfully, that later result remains authoritative and the failed compaction activity is still visible.
Only `agent_settled` settles the prompt; a compaction event or `agent_end` alone does not.
If cancellation is requested after a summary succeeds, the native settlement receipt can still confirm an abort; successful compaction does not override that confirmed cancellation.
Pre-prompt compaction can legitimately delay prompt admission beyond the ordinary RPC timeout, but remains bounded by the existing Agent Turn deadline and cancellation/cleanup budgets.

For repeated failures, inspect safe [diagnostics](health-and-diagnostics.md), the selected model, and the Participant's retained runtime image before using normal [troubleshooting](troubleshooting.md) and Human Handoff procedures.
Do not copy protected summaries or raw provider errors into public Issue comments, raise execution limits merely to hide a failure, or repeatedly reactivate a Workflow without addressing its cause.
Historical Human Handoffs, including #116, have not been established as context-exhaustion incidents.

## Runtime-image compatibility

Use a qualified image containing both Pi 1.4.0 and the compaction-enabled owned runner/resources.
Pi 1.4.0 fixes cancellation during pre-prompt compaction; the integration must not be transplanted onto the older 1.0.4 runtime.
New resource hashes are included through the existing owned-resource inventory and image qualification mechanisms.
Newly qualified images also include a deployment-owned compatibility hook for Pi 1.4.0's same-millisecond compaction/retry timestamp collision; it does not add another recovery attempt or change settlement policy.
Existing Participants keep their exact pinned runtime images and do not automatically acquire this capability when the controller is upgraded.
Do not rebind existing Participants or migrate historical failed Sessions as an upgrade shortcut.
Follow [upgrades](upgrades.md) and select only images with passing [both-backend qualification](../contributors/runtime-qualification.md).

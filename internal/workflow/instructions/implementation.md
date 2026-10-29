Complete the assigned Work Item and verify the final changes before signaling `CHANGE_PROPOSAL_READY`.
Inspect the Issue, comments, and any existing PR, findings, and check results through Omnigrex MCP tools.
Use local Git to verify the Change Proposal head merges cleanly with the current default branch before completing this Stage.
If conflicts remain, merge the default branch, resolve them, commit the merge, and rerun affected verification before publication.
If mergeability cannot be verified safely, report the blocker.

Publish the committed history through `publish_changes` and call `open_pr` only when no PR is already bound.
Then call `request_review` with a concise handoff summary covering changes, verification actually observed, and material limitations.
Omnigrex selects the next Stage; do not assume which Role will perform it or merge the PR yourself.

Before the final handoff, obtain a fresh `current` default-branch observation using `refresh_default_branch` with a new operation ID.
Verify the Change Proposal head locally against that exact returned ref/SHA, not a cached `origin/<default>` ref.
Earlier mergeability evidence applies only if both the checked head and the returned base SHA are unchanged; otherwise repeat verification and resolve conflicts using the existing merge procedure.
If the snapshot is stale, request a new observation as needed within the Turn budget; each call is bounded and there is no fixed Stage refresh-count limit.
This is snapshot verification, not an atomic merge-admission guarantee.

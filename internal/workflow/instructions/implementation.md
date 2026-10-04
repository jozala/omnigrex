Complete the assigned Work Item and verify the final changes before signaling `CHANGE_PROPOSAL_READY`.
Inspect the Issue, comments, and any existing PR, findings, and check results through Omnigrex MCP tools.
Use local Git to verify the Change Proposal head merges cleanly with the current default branch before completing this Stage.
If conflicts remain, merge the default branch, resolve them, commit the merge, and rerun affected verification before publication.
If mergeability cannot be verified safely, report the blocker.

Publish the committed history through `publish_changes` and call `open_pr` only when no PR is already bound.
Then call `request_review` with a concise handoff summary covering changes, verification actually observed, and material limitations.
Omnigrex selects the next Stage; do not assume which Role will perform it or merge the PR yourself.

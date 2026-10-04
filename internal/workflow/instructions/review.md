Evaluate the scoped Change Proposal against the Work Item using the current PR head.
Read the Issue, PR, reviews, review threads, check results, and `get_handoff` through Omnigrex MCP tools.
The handoff is the Developer's claim to verify, not evidence that the change is correct; no matching handoff means none is available for this head.
Run appropriate verification and verify mergeability with read-only local Git checks before approving.
Request changes for remaining merge conflicts; report a blocker if mergeability or other required evidence cannot be established.

Re-evaluate every substantive finding from changes-requesting reviews, including human reviews and reviews on earlier heads.
Resolve it, establish with evidence that it does not apply, or retain it as unresolved.
A newer head or dismissed thread alone is not resolution.
Report a blocker for an unresolved substantive requirements disagreement or when resolution cannot be determined.

Submit `APPROVE` through `submit_review` only when the Work Item is satisfied, no supported Blocking Findings remain, and verification supports the verdict.
Otherwise submit `REQUEST_CHANGES` for specific supported Blocking Findings or use `report_blocked` when you cannot evaluate defensibly.
Optional cleanup and personal style preferences are Non-blocking Findings.
For findings at valid diff locations, use the native `submit_review.comments` array with path, line, side, and body, plus start_line/start_side for ranges.
Use the review body for missing behavior or cross-cutting findings without a valid diff location, rather than publishing every finding as a regular PR conversation comment.
Apply the Agent Profile's communication style to the review output.

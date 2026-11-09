package agentevent

import "slices"

var mcpToolNames = []string{
	"assess_human_input", "complete_planning", "publish_plan_checkpoint", "get_collaboration_record", "get_issue", "list_issue_comments", "get_pull_request", "list_pull_request_reviews", "list_review_threads", "get_check_runs", "get_handoff", "get_check_run_diagnostics", "list_ci_runs", "get_ci_run", "get_ci_job_logs", "search_ci_job_logs", "refresh_default_branch", "publish_changes", "open_pr", "request_review", "submit_review", "comment_on_issue", "comment_on_pull_request", "request_clarification", "resolve_clarification", "report_blocked", "confirm_prior_terminal_intent",
}
var nativeToolNames = []string{"read", "edit", "write", "bash", "grep", "find", "ls"}

func KnownMCPToolName(name string) bool    { return slices.Contains(mcpToolNames, name) }
func KnownNativeToolName(name string) bool { return slices.Contains(nativeToolNames, name) }

package telemetry

import (
	"context"
	"slices"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// KnownMCPToolName checks the closed server/client vocabulary, kept in sync
// with the capability catalog by tests. It never expands the vocabulary.
func KnownMCPToolName(name string) bool { return slices.Contains(mcpToolNames, name) }

var mcpToolNames = []string{
	"complete_planning", "publish_plan_checkpoint", "get_collaboration_record", "get_issue", "list_issue_comments", "get_pull_request", "list_pull_request_reviews", "list_review_threads", "get_check_runs", "get_handoff", "get_check_run_diagnostics", "list_ci_runs", "get_ci_run", "get_ci_job_logs", "search_ci_job_logs", "refresh_default_branch", "publish_changes", "open_pr", "request_review", "submit_review", "comment_on_issue", "comment_on_pull_request", "request_clarification", "assess_human_input", "resolve_clarification", "report_blocked", "confirm_prior_terminal_intent",
}
var nativeToolNames = []string{"read", "edit", "write", "bash", "grep", "find", "ls"}

// RecordTool records one observed terminal call, not an admission or paired event
// ledger. Layers are independent populations: mcp_server, mcp_client, native.
// Missing duration omits the histogram, including for interrupted native calls.
// Unknown names/layers/outcomes are discarded, never exported as labels.
func RecordTool(ctx context.Context, layer, name string, outcome Outcome, duration *time.Duration) {
	if !metricsActive.Load() {
		return
	}
	switch layer {
	case "mcp_server", "mcp_client":
		if !KnownMCPToolName(name) {
			return
		}
	case "native":
		if !slices.Contains(nativeToolNames, name) {
			return
		}
	default:
		return
	}
	switch outcome {
	case Success, DomainOutcome, Failure, Cancelled, Timeout:
	default:
		return
	}
	meter := otel.Meter(Scope)
	calls, _ := meter.Int64Counter("omnigrex.tool.calls", metric.WithUnit("{call}"))
	elapsed, _ := meter.Float64Histogram("omnigrex.tool.duration", metric.WithUnit("s"))
	attrs := []attribute.KeyValue{attribute.String("layer", layer), attribute.String("tool", name)}
	calls.Add(ctx, 1, metric.WithAttributes(append(attrs, attribute.String("outcome", string(outcome)))...))
	if duration != nil && *duration >= 0 {
		elapsed.Record(ctx, duration.Seconds(), metric.WithAttributes(attrs...))
	}
}

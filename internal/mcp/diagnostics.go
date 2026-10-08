package mcp

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
)

type diagnosticContextKey struct{}

type callDiagnostic struct {
	id, workflowID, turnID string
	epoch                  int64
	started                time.Time
}

var diagnosticSequence atomic.Uint64
var diagnosticPrefix = newDiagnosticPrefix(rand.Reader)

func newDiagnosticPrefix(entropy io.Reader) string {
	var value [16]byte
	if _, err := io.ReadFull(entropy, value[:]); err == nil {
		return fmt.Sprintf("%x", value)
	}
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), os.Getpid())
}

func withCallDiagnostic(request *http.Request, scope TokenScope) *http.Request {
	diagnostic := callDiagnostic{
		id: fmt.Sprintf("%s-%x", diagnosticPrefix, diagnosticSequence.Add(1)), started: time.Now(),
		workflowID: scope.WorkflowID, turnID: scope.Lease.ID, epoch: scope.Lease.ExecutionEpoch,
	}
	return request.WithContext(context.WithValue(request.Context(), diagnosticContextKey{}, diagnostic))
}

func (gateway *Gateway) logRequestRejected(request *http.Request, tool, stage, reason string) {
	gateway.logCallFailure(request.Context(), "MCP request rejected", tool,
		readFailure{code: "request_rejected", stage: stage, reason: reason})
}

func (gateway *Gateway) logCallFailure(ctx context.Context, message, tool string, failure readFailure) {
	if gateway.logger == nil {
		return
	}
	diagnostic, ok := ctx.Value(diagnosticContextKey{}).(callDiagnostic)
	if !ok {
		return
	}
	failure = sanitizeReadFailure(failure)
	fields := []any{
		"workflow_id", diagnostic.workflowID, "agent_turn_id", diagnostic.turnID, "execution_epoch", diagnostic.epoch,
		"diagnostic_call_id", diagnostic.id, "duration_ms", time.Since(diagnostic.started).Milliseconds(),
		"failure_code", failure.code, "failure_stage", failure.stage, "failure_reason", failure.reason,
	}
	if known, found := definition(tool); found {
		fields = append(fields, "tool_name", known.Name)
	}
	if failure.field != "" {
		fields = append(fields, "validation_field", failure.field)
	}
	if failure.httpStatus != 0 {
		fields = append(fields, "github_http_status", failure.httpStatus)
	}
	if failure.requestID != "" {
		fields = append(fields, "github_request_id", failure.requestID)
	}
	gateway.logger.WarnContext(ctx, message, fields...)
}

func sanitizeReadFailure(failure readFailure) readFailure {
	detail := githubapi.SanitizeFailureDiagnostics(githubapi.FailureDiagnostics{
		Stage: failure.stage, Reason: failure.reason, Field: failure.field,
		HTTPStatus: failure.httpStatus, RequestID: failure.requestID,
	})
	if detail.Stage != "" {
		failure.stage = detail.Stage
	} else if !slices.Contains(gatewayFailureStages, failure.stage) {
		failure.stage = "unclassified"
	}
	if detail.Reason != "unclassified" {
		failure.reason = detail.Reason
	} else if !slices.Contains(gatewayFailureReasons, failure.reason) {
		failure.reason = "unclassified"
	}
	if !slices.Contains(readFailureCodes, failure.code) {
		failure.code = "unclassified"
	}
	failure.field, failure.httpStatus, failure.requestID = detail.Field, detail.HTTPStatus, detail.RequestID
	return failure
}

var gatewayFailureStages = strings.Fields(`request_validation initialization tool_authorization argument_validation turn_fence admission
	backend_execution scoped_identity_validation github_read credential_acquisition backend_result_validation read_recording ci_diagnostics_read`)

var gatewayFailureReasons = strings.Fields(`dependency_failed precondition_failed credential_unavailable invalid_protocol_version not_initialized
	invalid_accept unsupported_method invalid_content_type request_too_large trailing_json invalid_jsonrpc_version missing_method invalid_request_id
	unknown_method missing_request_id invalid_call_parameters invalid_metadata disallowed_tool unknown_tool invalid_arguments missing_turn_context
	stale_authorization invalid_result recording_failed unexpected_request_id invalid_parameters not_initializing invalid_list_parameters
	missing_capabilities missing_client_name missing_client_version pending missing expired unavailable unauthorized insufficient_permissions
	rate_limited transient scope_rejected invalid_continuation canceled deadline_exceeded resource_exhausted step_filter_unsupported unclassified`)

var readFailureCodes = strings.Fields(`request_rejected credential_unavailable canceled deadline_exceeded tool_dependency_failed read_precondition_failed
	backend_read_failed github_request_rejected github_graphql_errors_or_no_data github_invalid_response github_transport_failed github_read_failed
	backend_result_invalid read_recording_failed diagnostic_pending diagnostic_missing diagnostic_expired diagnostic_unavailable
	diagnostic_unauthorized diagnostic_rate_limited diagnostic_transient diagnostic_scope_rejected diagnostic_invalid_continuation
	diagnostic_deadline_exceeded diagnostic_resource_exhausted diagnostic_step_filter_unsupported unclassified`)

func fenceFailureReason(err error) string {
	if reason := githubapi.CancellationReason(err); reason != "" {
		return reason
	}
	if errors.Is(err, store.ErrAgentTurnFenceLost) {
		return "stale_authorization"
	}
	return "dependency_failed"
}

func readinessFailure(request *http.Request, registration *grant) string {
	if request.Header.Get("MCP-Protocol-Version") != ProtocolVersion {
		return "invalid_protocol_version"
	}
	registration.mutex.Lock()
	defer registration.mutex.Unlock()
	if !registration.initialized {
		return "not_initialized"
	}
	return ""
}

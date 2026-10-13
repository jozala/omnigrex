package github

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// FailureDiagnostics is an allowlisted snapshot, never a dependency error message.
type FailureDiagnostics struct {
	Stage, Reason, Field string
	HTTPStatus           int
	RequestID            string
}

// responseMetadata belongs to exactly one HTTP response, including absent headers.
type responseMetadata struct {
	header http.Header
	status int
}

func (metadata responseMetadata) requestID() string {
	return metadata.header.Get("X-GitHub-Request-Id")
}

type diagnosticError struct {
	cause    error
	detail   FailureDiagnostics
	response bool
}

func (err *diagnosticError) Error() string { return "GitHub operation failed" }
func (err *diagnosticError) Unwrap() error { return err.cause }

func validationError(reason, field string) error {
	return &diagnosticError{cause: ErrInvalidAPIResponse, detail: FailureDiagnostics{Reason: reason, Field: field}}
}

func responseFailure(metadata responseMetadata, stage, reason string, cause error) error {
	return &diagnosticError{cause: cause, response: true, detail: FailureDiagnostics{
		Stage: stage, Reason: reason, HTTPStatus: metadata.status, RequestID: metadata.requestID(),
	}}
}

func reviewThreadFailure(stage ReviewThreadReadStage, metadata responseMetadata, cause error) error {
	return &ReviewThreadReadError{Stage: stage, RequestID: metadata.requestID(),
		Cause: responseFailure(metadata, "", "", cause)}
}

// CancellationReason must be called before discarding a dependency's error chain.
func CancellationReason(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	default:
		return ""
	}
}

// SafeFailureDiagnostics validates even exported legacy error fields at the boundary.
// The first response carrier wins as a unit; absent metadata never falls back to an earlier response.
func SafeFailureDiagnostics(err error) FailureDiagnostics {
	var result FailureDiagnostics
	var staged *ReviewThreadReadError
	if errors.As(err, &staged) {
		switch staged.Stage {
		case ReviewThreadsQuery, ReviewThreadsValidation, ReviewCommentsQuery, ReviewCommentsValidation:
			result.Stage = string(staged.Stage)
		}
		result.RequestID = staged.RequestID
	}
	var graphql *GraphQLQueryError
	if errors.As(err, &graphql) && result.RequestID == "" {
		result.RequestID = graphql.RequestID
	}
	responseSeen := false
	for current := err; current != nil; current = errors.Unwrap(current) {
		diagnostic, ok := current.(*diagnosticError)
		if !ok {
			continue
		}
		detail := diagnostic.detail
		if detail.Stage != "" {
			result.Stage = detail.Stage
		}
		if detail.Reason != "" {
			result.Reason = detail.Reason
		}
		if detail.Field != "" {
			result.Field = detail.Field
		}
		if !responseSeen && diagnostic.response {
			result.HTTPStatus, result.RequestID = detail.HTTPStatus, detail.RequestID
			responseSeen = true
		}
	}
	if !responseSeen {
		var api *APIError
		var permission *PermissionError
		var rateLimit *RateLimitError
		switch {
		case errors.As(err, &api):
		case errors.As(err, &permission):
			api = permission.APIError
		case errors.As(err, &rateLimit):
			api = rateLimit.APIError
		}
		if api != nil {
			result.HTTPStatus, result.RequestID = api.StatusCode, api.RequestID
		}
	}
	// Cancellation and transport/rejection reasons apply across operations.
	// Keep the operation identity rather than replacing it with the inner HTTP phase.
	if staged != nil {
		switch result.Reason {
		case "canceled", "deadline_exceeded", "transport_failed", "http_rejected":
			result.Stage = string(staged.Stage)
		}
	}
	return SanitizeFailureDiagnostics(result)
}

// SanitizeFailureDiagnostics revalidates snapshots at downstream export boundaries.
func SanitizeFailureDiagnostics(result FailureDiagnostics) FailureDiagnostics {
	if result.HTTPStatus < 100 || result.HTTPStatus > 599 {
		result.HTTPStatus = 0
	}
	if !safeDiagnosticRequestID(result.RequestID) {
		result.RequestID = ""
	}
	if !slices.Contains(diagnosticStages, result.Stage) {
		result.Stage = ""
	}
	if !slices.Contains(diagnosticReasons, result.Reason) {
		result.Reason = "unclassified"
	}
	if !slices.Contains(diagnosticFields, result.Field) {
		result.Field = ""
	}
	return result
}

var diagnosticStages = strings.Fields(`review_threads_query review_threads_validation review_comments_query review_comments_validation
	issue_validation github_http response_decoding graphql_envelope graphql_data_decoding
	ci_check_validation ci_run_validation ci_job_validation ci_logs_location ci_logs_download ci_scope_validation`)

var diagnosticReasons = strings.Fields(`missing_field invalid_node_id identity_mismatch identity_changed invalid_count count_mismatch count_changed
	oversized_page repeated_cursor invalid_cursor empty_continuation empty_page_has_cursor empty_page_has_next duplicate_id invalid_path
	invalid_diff_side invalid_subject_type file_has_line file_has_diff_side invalid_line invalid_range outdated_has_line
	single_line_has_diff_side single_line_has_start_line invalid_position position_on_file location_mismatch invalid_id invalid_state invalid_url
	invalid_commit invalid_identity invalid_timestamp timestamp_order missing_target root_is_reply reply_not_to_root
	canceled deadline_exceeded transport_failed http_rejected invalid_json graphql_errors missing_data unclassified`)

var diagnosticFields = strings.Fields(`repository repository.id repository.name repository.owner repository.owner.login
	pull_request pull_request.id pull_request.number pull_request.repository pull_request.repository.id pull_request.repository.name
	pull_request.repository.owner pull_request.repository.owner.login review_threads review_threads.totalCount review_threads.nodes
	review_thread review_thread.id review_thread.isResolved review_thread.isOutdated review_thread.path review_thread.diffSide
	review_thread.startDiffSide review_thread.line review_thread.startLine review_thread.originalLine review_thread.originalStartLine review_thread.subjectType
	review_comments review_comments.totalCount review_comments.nodes review_comment review_comment.id review_comment.fullDatabaseId
	review_comment.body review_comment.path review_comment.commit review_comment.commit.oid review_comment.originalCommit review_comment.originalCommit.oid
	review_comment.url review_comment.author review_comment.author.login review_comment.createdAt review_comment.updatedAt
	review_comment.pullRequestReview.fullDatabaseId review_comment.position review_comment.subjectType review_comment.line review_comment.startLine
	review_comment.originalLine review_comment.originalStartLine review_comment.replyTo review_comment.replyTo.id review_comment.replyTo.fullDatabaseId
	page_info page_info.hasNextPage page_info.hasPreviousPage page_info.startCursor page_info.endCursor
	issue.id issue.node_id issue.number issue.title issue.state issue.html_url`)

// safeDiagnosticRequestID accepts GitHub's colon-separated hexadecimal request IDs only.
func safeDiagnosticRequestID(value string) bool {
	groups := strings.Split(value, ":")
	if len(groups) != 5 {
		return false
	}
	for _, group := range groups {
		if len(group) < 3 || len(group) > 12 || strings.Trim(group, "0123456789ABCDEF") != "" {
			return false
		}
	}
	return true
}

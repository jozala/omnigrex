package mcp

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/workspace"
)

// FailurePullRequestHeadMismatch is durable evidence of a definite, fresh
// identity/head conflict, unlike an unavailable GitHub observation.
const FailurePullRequestHeadMismatch = "pull_request_head_mismatch"

// FailurePublicationRemoteHeadMismatch is definite Git compare-and-swap
// evidence that a recovered head is no longer current.
const FailurePublicationRemoteHeadMismatch = "publication_remote_head_mismatch"

// mutationFailure contains only gateway-authored, credential-free diagnostics.
// Never construct it from a dependency's error string or response body.
type mutationFailure struct {
	code        string
	message     string
	httpCode    int
	requestID   string
	expectedSHA string
	observedSHA string
}

func publicationHeadFailure(err error) mutationFailure {
	failure := mutationFailure{code: FailurePublicationRemoteHeadMismatch, message: "publication remote head mismatch"}
	var mismatch *workspace.UnexpectedHeadError
	if errors.As(err, &mismatch) {
		failure.expectedSHA, failure.observedSHA = mismatch.Expected, mismatch.Actual
	}
	return failure
}

func (failure mutationFailure) Error() string {
	fields := failure.safeFields()
	if len(fields) == 0 {
		return failure.code
	}
	return failure.code + " " + strings.Join(fields, " ")
}

func (failure mutationFailure) toolMessage() string {
	fields := failure.safeFields()
	if len(fields) == 0 {
		return failure.message
	}
	return failure.message + " (" + strings.Join(fields, ", ") + ")"
}

// Only typed scalar fields go into durable and agent-facing diagnostics.
func (failure mutationFailure) safeFields() []string {
	var fields []string
	if failure.httpCode >= 400 && failure.httpCode <= 599 {
		fields = append(fields, "http_status="+strconv.Itoa(failure.httpCode))
	}
	if safeRequestID(failure.requestID) {
		fields = append(fields, "request_id="+failure.requestID)
	}
	if (failure.expectedSHA != "" || failure.observedSHA != "") && (failure.expectedSHA == "" || validRevision(failure.expectedSHA)) {
		if failure.observedSHA == "" || validRevision(failure.observedSHA) {
			if failure.code == FailurePublicationRemoteHeadMismatch {
				fields = append(fields, fmt.Sprintf("expected_sha=%s", failure.expectedSHA), fmt.Sprintf("observed_sha=%s", failure.observedSHA))
			}
		}
	}
	return fields
}

func safeRequestID(value string) bool {
	return value != "" && len(value) <= 128 && strings.Trim(value, "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-:_") == ""
}

// A cached failure may predate safe diagnostics or contain data from an old
// implementation. Only replay canonical, known gateway-authored fields.
func cachedFailureMessage(lastError string) string {
	const fallback = "mutation previously failed"
	if len(lastError) > 512 {
		return fallback
	}
	parts := strings.Fields(lastError)
	if len(parts) == 0 {
		return fallback
	}
	var failure mutationFailure
	switch parts[0] {
	case FailurePublicationRemoteHeadMismatch:
		failure = mutationFailure{code: parts[0], message: "publication remote head mismatch"}
	case "github_request_rejected":
		failure = mutationFailure{code: parts[0], message: "GitHub rejected the mutation"}
	case "github_observation_unavailable":
		failure = mutationFailure{code: parts[0], message: "could not verify the Pull Request head"}
	case FailurePullRequestHeadMismatch:
		failure = mutationFailure{code: parts[0], message: "Pull Request head or identity changed during this turn"}
	case "pull_request_context_missing":
		failure = mutationFailure{code: parts[0], message: "Pull Request context is missing for this turn"}
	case "pull_request_already_bound":
		failure = mutationFailure{code: parts[0], message: "Pull Request is already bound to this turn"}
	case "github_dependency_failed":
		failure = mutationFailure{code: parts[0], message: "GitHub mutation failed"}
	case "publication_tree_mismatch":
		failure = mutationFailure{code: parts[0], message: "workspace and publication trees differ"}
	case "publication_dependency_failed":
		failure = mutationFailure{code: parts[0], message: "publication could not be prepared"}
	case "invalid_tool_invocation":
		failure = mutationFailure{code: parts[0], message: "invalid tool invocation"}
	case "tool_not_authorized":
		failure = mutationFailure{code: parts[0], message: "tool is not authorized"}
	case "tool_precondition_failed":
		failure = mutationFailure{code: parts[0], message: "tool precondition failed"}
	case "tool_dependency_failed":
		failure = mutationFailure{code: parts[0], message: "tool dependency failed"}
	case "backend_result_invalid":
		failure = mutationFailure{code: parts[0], message: "mutation returned an invalid result"}
	default:
		return fallback
	}
	for _, part := range parts[1:] {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return fallback
		}
		switch key {
		case "http_status":
			if failure.code != "github_request_rejected" && failure.code != "github_observation_unavailable" {
				return fallback
			}
			status, err := strconv.Atoi(value)
			if err != nil {
				return fallback
			}
			failure.httpCode = status
		case "request_id":
			if failure.code != "github_request_rejected" && failure.code != "github_observation_unavailable" {
				return fallback
			}
			failure.requestID = value
		case "expected_sha":
			if failure.code != FailurePublicationRemoteHeadMismatch {
				return fallback
			}
			failure.expectedSHA = value
		case "observed_sha":
			if failure.code != FailurePublicationRemoteHeadMismatch {
				return fallback
			}
			failure.observedSHA = value
		default:
			return fallback
		}
	}
	if failure.Error() != lastError {
		return fallback
	}
	return failure.toolMessage()
}

func safeMutationFailure(err error) mutationFailure {
	var classified mutationFailure
	if errors.As(err, &classified) {
		return classified
	}
	switch {
	case errors.Is(err, ErrInvalidInvocation):
		return mutationFailure{code: "invalid_tool_invocation", message: "invalid tool invocation"}
	case errors.Is(err, ErrToolNotAuthorized):
		return mutationFailure{code: "tool_not_authorized", message: "tool is not authorized"}
	case errors.Is(err, ErrToolPrecondition):
		return mutationFailure{code: "tool_precondition_failed", message: "tool precondition failed"}
	case errors.Is(err, ErrToolDependency):
		return mutationFailure{code: "tool_dependency_failed", message: "tool dependency failed"}
	default:
		return mutationFailure{code: "backend_failure_unclassified", message: "mutation failed"}
	}
}

func githubMutationFailure(err error) mutationFailure {
	httpCode, requestID := githubFailureMetadata(err)
	if httpCode == 0 {
		return mutationFailure{code: "github_dependency_failed", message: "GitHub mutation failed"}
	}
	return mutationFailure{code: "github_request_rejected", message: "GitHub rejected the mutation", httpCode: httpCode, requestID: requestID}
}

func githubObservationFailure(err error) mutationFailure {
	httpCode, requestID := githubFailureMetadata(err)
	return mutationFailure{
		code: "github_observation_unavailable", message: "could not verify the Pull Request head",
		httpCode: httpCode, requestID: requestID,
	}
}

func githubFailureMetadata(err error) (int, string) {
	var api *githubapi.APIError
	if !errors.As(err, &api) {
		var permission *githubapi.PermissionError
		var rateLimit *githubapi.RateLimitError
		switch {
		case errors.As(err, &permission):
			api = permission.APIError
		case errors.As(err, &rateLimit):
			api = rateLimit.APIError
		}
	}
	if api == nil || api.StatusCode < 400 || api.StatusCode > 599 {
		return 0, ""
	}
	if safeRequestID(api.RequestID) {
		return api.StatusCode, api.RequestID
	}
	return api.StatusCode, ""
}

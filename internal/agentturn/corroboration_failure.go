package agentturn

import (
	"context"
	"errors"
	"net/http"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

// corroborationFailure contains only fixed classifications and typed status
// metadata. In particular, it never retains a dependency's error message.
type corroborationFailure struct {
	code         string
	retryable    bool
	prerequisite bool
	status       int
}

func classifyGitHubCorroborationFailure(err error) corroborationFailure {
	failure := corroborationFailure{code: "github_dependency_unknown", retryable: true}
	var rateLimit *githubapi.RateLimitError
	var permission *githubapi.PermissionError
	var notInstalled *githubapi.NotInstalledError
	var api *githubapi.APIError
	var transient *githubapi.TransientError
	var configuration *githubapi.ConfigurationError
	switch {
	case errors.Is(err, context.Canceled):
		failure.code, failure.retryable = "cancelled", false
	case errors.Is(err, context.DeadlineExceeded):
		failure.code = "deadline_exceeded"
	case errors.Is(err, githubapi.ErrInvalidAPIResponse):
		failure.code = "invalid_api_response"
	case errors.As(err, &rateLimit):
		failure.code, api = "rate_limited", rateLimit.APIError
	case errors.As(err, &permission):
		failure.code, failure.retryable, failure.prerequisite, api = "permission_denied", false, true, permission.APIError
	case errors.As(err, &notInstalled):
		failure.code, failure.retryable, failure.prerequisite = "permission_denied", false, true
	case errors.As(err, &api):
		failure.code = "github_http_error"
	case errors.As(err, &transient):
		failure.code = "transient_transport"
	case errors.As(err, &configuration):
		failure.code, failure.retryable, failure.prerequisite = "invalid_configuration", false, true
	default:
		metadata := githubapi.ExtractSafeErrorMetadata(err)
		if metadata.Permanent {
			failure.code, failure.retryable, failure.prerequisite = "github_prerequisite_unavailable", false, true
		} else if metadata.Transient {
			if metadata.RetryAfter > 0 || metadata.ResetAt.After(time.Now()) {
				failure.code = "rate_limited"
			}
		} else if metadata.APIClientError && !metadata.APIRetryable {
			failure.code, failure.retryable, failure.prerequisite = "github_prerequisite_unavailable", false, true
		}
	}
	if api == nil || api.StatusCode < http.StatusBadRequest || api.StatusCode > 599 {
		return failure
	}
	failure.status = api.StatusCode
	if rateLimit != nil {
		return failure
	}
	switch api.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		failure.retryable, failure.prerequisite = false, true
	case http.StatusNotFound, http.StatusRequestTimeout, http.StatusTooManyRequests:
		failure.retryable = true
	default:
		failure.retryable = api.StatusCode >= http.StatusInternalServerError
		failure.prerequisite = !failure.retryable
	}
	return failure
}

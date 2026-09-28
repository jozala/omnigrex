package agentturn

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

func TestClassifyGitHubCorroborationFailure(t *testing.T) {
	for _, test := range []struct {
		name, code   string
		err          error
		retryable    bool
		prerequisite bool
		status       int
	}{
		{name: "transient transport", err: &githubapi.TransientError{Cause: errors.New("secret in transport")}, code: "transient_transport", retryable: true},
		{name: "deadline", err: fmt.Errorf("transport: %w", context.DeadlineExceeded), code: "deadline_exceeded", retryable: true},
		{name: "cancelled", err: context.Canceled, code: "cancelled"},
		{name: "invalid response", err: fmt.Errorf("body: %w", githubapi.ErrInvalidAPIResponse), code: "invalid_api_response", retryable: true},
		{name: "new PR not visible", err: &githubapi.APIError{StatusCode: 404}, code: "github_http_error", retryable: true, status: 404},
		{name: "server failure", err: &githubapi.APIError{StatusCode: 503}, code: "github_http_error", retryable: true, status: 503},
		{name: "bad request", err: &githubapi.APIError{StatusCode: 422}, code: "github_http_error", prerequisite: true, status: 422},
		{name: "unauthorized", err: &githubapi.APIError{StatusCode: 401}, code: "github_http_error", prerequisite: true, status: 401},
		{name: "permission wrapper", err: &githubapi.PermissionError{APIError: &githubapi.APIError{StatusCode: 403}}, code: "permission_denied", prerequisite: true, status: 403},
		{name: "installation removed", err: &githubapi.NotInstalledError{Owner: "owner", Repository: "repo"}, code: "permission_denied", prerequisite: true},
		{name: "permission without API metadata", err: &githubapi.PermissionError{}, code: "permission_denied", prerequisite: true},
		{name: "rate limit at 403", err: &githubapi.RateLimitError{APIError: &githubapi.APIError{StatusCode: 403}}, code: "rate_limited", retryable: true, status: 403},
		{name: "rate limit at 429", err: &githubapi.RateLimitError{APIError: &githubapi.APIError{StatusCode: 429}}, code: "rate_limited", retryable: true, status: 429},
		{name: "configuration", err: &githubapi.ConfigurationError{Cause: errors.New("secret")}, code: "invalid_configuration", prerequisite: true},
		{name: "redacted permission metadata", err: safeMetadataError{githubapi.SafeErrorMetadata{Permanent: true, APIClientError: true}}, code: "github_prerequisite_unavailable", prerequisite: true},
		{name: "redacted rate limit metadata", err: safeMetadataError{githubapi.SafeErrorMetadata{Transient: true, APIClientError: true, RetryAfter: time.Minute}}, code: "rate_limited", retryable: true},
		{name: "unknown dependency", err: errors.New("secret"), code: "github_dependency_unknown", retryable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := classifyGitHubCorroborationFailure(test.err)
			if failure.code != test.code || failure.retryable != test.retryable || failure.prerequisite != test.prerequisite || failure.status != test.status {
				t.Fatalf("classification = %#v, want code %q retryable %v prerequisite %v status %d", failure, test.code, test.retryable, test.prerequisite, test.status)
			}
			if failure.retryable && failure.prerequisite {
				t.Fatal("failure is both retryable and a permanent prerequisite")
			}
		})
	}
}

type safeMetadataError struct{ metadata githubapi.SafeErrorMetadata }

func (safeMetadataError) Error() string                                      { return "redacted GitHub credential failure" }
func (err safeMetadataError) SafeErrorMetadata() githubapi.SafeErrorMetadata { return err.metadata }

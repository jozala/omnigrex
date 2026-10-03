package github

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDiagnosticExportDiscardsUntrustedValues(t *testing.T) {
	err := &diagnosticError{response: true, cause: ErrInvalidAPIResponse, detail: FailureDiagnostics{
		Stage: "secret-stage", Reason: "secret-reason", Field: "secret-path", HTTPStatus: 999, RequestID: "secret-header",
	}}
	if got := SafeFailureDiagnostics(err); got != (FailureDiagnostics{Reason: "unclassified"}) {
		t.Fatalf("untrusted diagnostics exported: %+v", got)
	}
	wrapped := &ReviewThreadReadError{Stage: "secret-stage", RequestID: "secret-header", Cause: err}
	if got := SafeFailureDiagnostics(wrapped); got != (FailureDiagnostics{Reason: "unclassified"}) {
		t.Fatalf("untrusted legacy diagnostics exported: %+v", got)
	}
}

func TestDiagnosticRedactionPreservesRetryAndSentinels(t *testing.T) {
	metadata := responseMetadata{status: 403, header: http.Header{"X-Github-Request-Id": []string{"A41A:2AF0FB:6AAAD0:6A71D9:6ABC2962"}}}
	for _, cause := range []error{
		&PermissionError{APIError: &APIError{StatusCode: 403, Message: "credential-sentinel"}},
		&RateLimitError{APIError: &APIError{StatusCode: 429, Message: "credential-sentinel"}, RetryAfter: time.Minute, ResetAt: time.Now().UTC()},
		&TransientError{Cause: &APIError{StatusCode: 502, Message: "credential-sentinel"}},
		validationError("invalid_state", "issue.state"), context.Canceled,
	} {
		original := responseFailure(metadata, "issue_validation", "", cause)
		redacted := redactAPIClientError(original, "credential-sentinel")
		if !reflect.DeepEqual(ExtractSafeErrorMetadata(original), ExtractSafeErrorMetadata(redacted)) || SafeFailureDiagnostics(original) != SafeFailureDiagnostics(redacted) {
			t.Fatalf("redaction lost retry/diagnostic metadata for %T", cause)
		}
		for _, sentinel := range []error{ErrInvalidAPIResponse, context.Canceled, context.DeadlineExceeded} {
			if errors.Is(original, sentinel) != errors.Is(redacted, sentinel) {
				t.Fatalf("redaction changed %v", sentinel)
			}
		}
		for current := redacted; current != nil; current = errors.Unwrap(current) {
			if strings.Contains(current.Error(), "credential-sentinel") {
				t.Fatalf("credential in redacted chain")
			}
		}
	}
}

func TestResponseMetadataIsAtomicEvenWhenAbsent(t *testing.T) {
	previous := responseFailure(responseMetadata{status: 200, header: http.Header{"X-Github-Request-Id": []string{"A41A:2AF0FB:6AAAD0:6A71D9:6ABC2962"}}}, "", "", ErrInvalidAPIResponse)
	err := responseFailure(responseMetadata{}, "github_http", "transport_failed", previous)
	got := SafeFailureDiagnostics(err)
	if got.HTTPStatus != 0 || got.RequestID != "" {
		t.Fatalf("inherited unrelated response: %+v", got)
	}
}

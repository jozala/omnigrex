package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	githubapi "github.com/jozala/omnigrex/internal/github"
)

type diagnosticDoer func(*http.Request) (*http.Response, error)

func (do diagnosticDoer) Do(request *http.Request) (*http.Response, error) { return do(request) }

func TestSuccessfulResponseMetadataDoesNotChangeMutationClassification(t *testing.T) {
	client, err := githubapi.NewAPIClient(diagnosticDoer(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"X-Github-Request-Id": []string{"A41A:2AF0FB:6AAAD0:6A71D9:6ABC2962"}}, Body: io.NopCloser(strings.NewReader(`{"id":0}`))}, nil
	}), "https://github.test")
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetIssue(context.Background(), "credential-sentinel", "acme", "widgets", 12)
	if err == nil {
		t.Fatal("expected validation failure")
	}
	read := safeReadFailure(newGitHubReadError(err))
	if read.code != "github_invalid_response" || read.httpStatus != 200 {
		t.Fatalf("read = %+v", read)
	}
	mutation := githubMutationFailure(err)
	if mutation.code != "github_dependency_failed" || mutation.httpCode != 0 || mutation.requestID != "" {
		t.Fatalf("mutation changed: %+v", mutation)
	}
	observation := githubObservationFailure(err)
	if observation.httpCode != 0 || observation.requestID != "" {
		t.Fatalf("observation changed: %+v", observation)
	}
}

func TestDiagnosticPrefixHasNonFailingFallback(t *testing.T) {
	if prefix := newDiagnosticPrefix(strings.NewReader("")); prefix == "" {
		t.Fatal("missing fallback prefix")
	}
}

func TestFinalLogBoundaryDiscardsUntrustedDiagnosticValues(t *testing.T) {
	var logs bytes.Buffer
	gateway := &Gateway{logger: slog.New(slog.NewJSONHandler(&logs, nil))}
	request := withCallDiagnostic(httptest.NewRequest(http.MethodPost, "/mcp", nil), TokenScope{})
	gateway.logCallFailure(request.Context(), "MCP read failed", "secret-tool", readFailure{
		code: "secret-code", stage: "secret-stage", reason: "secret-reason", field: "secret-field", requestID: "secret-header", httpStatus: 999,
	})
	if strings.Contains(logs.String(), "secret-") {
		t.Fatalf("diagnostic values escaped the final allowlist: %s", &logs)
	}
	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"failure_code", "failure_stage", "failure_reason"} {
		if entry[key] != "unclassified" {
			t.Fatalf("missing fallback: %s", &logs)
		}
	}
	for _, key := range []string{"tool_name", "validation_field", "github_http_status", "github_request_id"} {
		if _, exists := entry[key]; exists {
			t.Fatalf("untrusted optional field: %s", &logs)
		}
	}
}

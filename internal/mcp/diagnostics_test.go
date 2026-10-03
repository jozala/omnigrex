package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func diagnosticGateway(t *testing.T, backend mcp.Backend, durable *fakeStore, ledger mcp.ReadLedger, logs io.Writer) (*mcp.Gateway, mcp.Registration) {
	t.Helper()
	now := time.Now()
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend, Ledger: ledger,
		Logger: slog.New(slog.NewJSONHandler(logs, nil)), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScope(now)
	scope.HeadSHA = productionHeadSHA
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatal(err)
	}
	initialize(t, gateway, registration)
	return gateway, registration
}

func diagnosticEntries(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var entries []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(logs.Bytes()))
	for {
		var entry map[string]any
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	for _, secret := range []string{"private-sentinel", "credential-sentinel", "lease-secret"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs disclosed %s", secret)
		}
	}
	return entries
}

const diagnosticCall = `{"jsonrpc":"2.0","id":"private-sentinel","method":"tools/call","params":{"name":"get_issue","arguments":{}}}`

func TestAuthenticatedRejectionDiagnostics(t *testing.T) {
	for _, test := range []struct {
		name, body, stage, reason, tool string
		status                          int
		modify                          func(*http.Request)
		fence                           error
	}{
		{name: "malformed", body: `{"private-sentinel":`, stage: "request_validation", reason: "invalid_json", status: 400},
		{name: "trailing", body: diagnosticCall + `{}`, stage: "request_validation", reason: "trailing_json", status: 400},
		{name: "version", body: `{"jsonrpc":"1.0","method":"tools/call"}`, stage: "request_validation", reason: "invalid_jsonrpc_version", status: 400},
		{name: "missing method", body: `{"jsonrpc":"2.0"}`, stage: "request_validation", reason: "missing_method", status: 400},
		{name: "invalid ID", body: `{"jsonrpc":"2.0","id":{},"method":"tools/call"}`, stage: "request_validation", reason: "invalid_request_id", status: 200},
		{name: "missing ID", body: `{"jsonrpc":"2.0","method":"tools/call"}`, stage: "request_validation", reason: "missing_request_id", status: 400},
		{name: "unknown tool", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"private-sentinel","arguments":{}}}`, stage: "tool_authorization", reason: "unknown_tool", status: 200},
		{name: "disallowed tool", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"submit_review","arguments":{}}}`, stage: "tool_authorization", reason: "disallowed_tool", tool: "submit_review", status: 200},
		{name: "unknown parameter", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_issue","private-sentinel":true}}`, stage: "request_validation", reason: "invalid_call_parameters", status: 200},
		{name: "metadata", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_issue","_meta":null}}`, stage: "request_validation", reason: "invalid_metadata", tool: "get_issue", status: 200},
		{name: "arguments", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_issue","arguments":{"private-sentinel":true}}}`, stage: "argument_validation", reason: "invalid_arguments", tool: "get_issue", status: 200},
		{name: "context", body: `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_review_threads","arguments":{}}}`, stage: "tool_authorization", reason: "missing_turn_context", tool: "list_review_threads", status: 200},
		{name: "protocol", stage: "initialization", reason: "invalid_protocol_version", status: 400, modify: func(r *http.Request) { r.Header.Set("MCP-Protocol-Version", "private-sentinel") }},
		{name: "accept", stage: "request_validation", reason: "invalid_accept", status: 406, modify: func(r *http.Request) { r.Header.Set("Accept", "private-sentinel") }},
		{name: "content type", stage: "request_validation", reason: "invalid_content_type", status: 415, modify: func(r *http.Request) { r.Header.Set("Content-Type", "private-sentinel") }},
		{name: "path", stage: "request_validation", reason: "invalid_path", status: 404, modify: func(r *http.Request) { r.URL.Path = "/private-sentinel" }},
		{name: "method", stage: "request_validation", reason: "unsupported_method", status: 405, modify: func(r *http.Request) { r.Method = "PUT" }},
		{name: "fence lost", stage: "turn_fence", reason: "stale_authorization", status: 401, fence: store.ErrAgentTurnFenceLost},
		{name: "fence dependency", stage: "turn_fence", reason: "dependency_failed", status: 401, fence: errors.New("private-sentinel")},
		{name: "fence cancellation", stage: "turn_fence", reason: "canceled", status: 401, fence: context.Canceled},
		{name: "fence deadline", stage: "turn_fence", reason: "deadline_exceeded", status: 401, fence: context.DeadlineExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			backend := &recordingBackend{}
			ledger := &recordingLedger{}
			durable := &fakeStore{}
			gateway, registration := diagnosticGateway(t, backend, durable, ledger, &logs)
			durable.setValidateError(test.fence)
			body := test.body
			if body == "" {
				body = diagnosticCall
			}
			request := rpcRequest(t, registration, mcp.ProtocolVersion, body)
			if test.modify != nil {
				test.modify(request)
			}
			response := httptest.NewRecorder()
			gateway.ServeHTTP(response, request)
			if response.Code != test.status || backend.count() != 0 || ledger.count() != 0 {
				t.Fatalf("response=%d backend=%d ledger=%d", response.Code, backend.count(), ledger.count())
			}
			entries := diagnosticEntries(t, &logs)
			if len(entries) != 1 {
				t.Fatalf("entries = %v", entries)
			}
			entry := entries[0]
			if entry["msg"] != "MCP request rejected" || entry["failure_stage"] != test.stage || entry["failure_reason"] != test.reason || entry["diagnostic_call_id"] == "" || entry["agent_turn_id"] != "turn-1" {
				t.Fatalf("entry = %v", entry)
			}
			if test.tool == "" {
				if _, ok := entry["tool_name"]; ok {
					t.Fatalf("unexpected tool: %v", entry)
				}
			} else if entry["tool_name"] != test.tool {
				t.Fatalf("tool: %v", entry)
			}
			if _, ok := entry["github_http_status"]; ok {
				t.Fatalf("unknown HTTP status emitted: %v", entry)
			}
		})
	}
}

func TestReadFailureCorrelationAndEventCounts(t *testing.T) {
	for _, test := range []struct {
		name               string
		result             json.RawMessage
		cause, ledgerError error
		code               string
	}{
		{"backend and ledger", nil, errors.New("private-sentinel"), errors.New("private-sentinel"), "backend_read_failed"},
		{"canceled", nil, context.Canceled, nil, "canceled"},
		{"deadline", nil, context.DeadlineExceeded, nil, "deadline_exceeded"},
		{"malformed", json.RawMessage(`{"private-sentinel":`), nil, nil, "backend_result_invalid"},
		{"empty", nil, nil, nil, "backend_result_invalid"},
		{"recording only", json.RawMessage(`{}`), nil, errors.New("private-sentinel"), "read_recording_failed"},
		{"success", json.RawMessage(`{}`), nil, nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			backend := &recordingBackend{result: test.result, err: test.cause}
			ledger := &recordingLedger{err: test.ledgerError}
			gateway, registration := diagnosticGateway(t, backend, &fakeStore{}, ledger, &logs)
			gateway.ServeHTTP(httptest.NewRecorder(), rpcRequest(t, registration, mcp.ProtocolVersion, diagnosticCall))
			entries := diagnosticEntries(t, &logs)
			wantCount := 1
			if test.code == "" {
				wantCount = 0
			}
			if test.cause != nil && test.ledgerError != nil {
				wantCount++
			}
			if len(entries) != wantCount {
				t.Fatalf("entries=%v", entries)
			}
			for i, entry := range entries {
				if entry["msg"] != "MCP read failed" || entry["diagnostic_call_id"] == "" || entry["diagnostic_call_id"] != entries[0]["diagnostic_call_id"] {
					t.Fatalf("uncorrelated: %v", entries)
				}
				if i == 0 && entry["failure_code"] != test.code {
					t.Fatalf("classification: %v", entry)
				}
				if i > 0 && (entry["failure_stage"] != "read_recording" || entry["duration_ms"].(float64) < entries[0]["duration_ms"].(float64)) {
					t.Fatalf("recording: %v", entry)
				}
				if _, ok := entry["github_http_status"]; ok {
					t.Fatalf("unknown status: %v", entry)
				}
			}
		})
	}
}

func TestConcurrentCallsHaveDistinctDiagnosticIDs(t *testing.T) {
	var logs bytes.Buffer // slog's JSON handler serializes writes.
	gateway, registration := diagnosticGateway(t, &recordingBackend{err: context.Canceled}, &fakeStore{}, nil, &logs)
	var group sync.WaitGroup
	for range 20 {
		request := rpcRequest(t, registration, mcp.ProtocolVersion, diagnosticCall)
		group.Go(func() { gateway.ServeHTTP(httptest.NewRecorder(), request) })
	}
	group.Wait()
	entries := diagnosticEntries(t, &logs)
	ids := make(map[any]bool)
	for _, entry := range entries {
		if entry["diagnostic_call_id"] == "" || ids[entry["diagnostic_call_id"]] {
			t.Fatalf("duplicate ID: %v", entry)
		}
		ids[entry["diagnostic_call_id"]] = true
	}
	if len(ids) != 20 {
		t.Fatalf("got %d IDs", len(ids))
	}
}

func TestAuthenticationBlindSpotRemainsQuiet(t *testing.T) {
	var logs bytes.Buffer
	gateway, registration := diagnosticGateway(t, &recordingBackend{}, &fakeStore{}, nil, &logs)
	gateway.Revoke(registration)
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, diagnosticCall))
	if response.Code != 401 || logs.Len() != 0 {
		t.Fatalf("response=%d logs=%s", response.Code, &logs)
	}
}

func TestProductionReadDiagnosticsAcrossSanitization(t *testing.T) {
	for _, test := range []struct {
		name, code, stage string
		credentialError   error
		mismatch          bool
	}{
		{"credential cancel", "canceled", "credential_acquisition", context.Canceled, false},
		{"credential deadline", "deadline_exceeded", "credential_acquisition", context.DeadlineExceeded, false},
		{"scoped issue", "read_precondition_failed", "scoped_identity_validation", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &diagnosticGitHub{issueID: 999}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api,
				Credentials: &backendCredentials{developer: "credential-sentinel", err: test.credentialError}, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{}})
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			gateway, registration := diagnosticGateway(t, backend, &fakeStore{}, nil, &logs)
			gateway.ServeHTTP(httptest.NewRecorder(), rpcRequest(t, registration, mcp.ProtocolVersion, diagnosticCall))
			entries := diagnosticEntries(t, &logs)
			if len(entries) != 1 || entries[0]["failure_code"] != test.code || entries[0]["failure_stage"] != test.stage {
				t.Fatalf("entries=%v", entries)
			}
			if _, ok := entries[0]["github_http_status"]; ok {
				t.Fatalf("unknown status: %v", entries)
			}
			if test.mismatch {
				if entries[0]["failure_reason"] != "identity_mismatch" {
					t.Fatalf("mismatch: %v", entries)
				}
				scope := productionToolScope(workflow.RoleDeveloper)
				_, err := backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolGetIssue, Class: mcp.ReadTool, Scope: scope, Arguments: json.RawMessage(`{}`)})
				if !errors.Is(err, mcp.ErrToolPrecondition) {
					t.Fatalf("lost precondition: %v", err)
				}
			} else if len(api.calls) != 0 {
				t.Fatalf("credential failure reached API")
			}
		})
	}
}

func TestSuccessfulGitHubResponseFailureReachesGateway(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-GitHub-Request-Id", "A41A:2AF0FB:6AAAD0:6A71D9:6ABC2962")
		_, _ = io.WriteString(w, `{"id":123,"node_id":"I_123","number":12,"title":"private-sentinel","state":"invalid","html_url":"https://github.test/acme/widgets/issues/12"}`)
	}))
	defer server.Close()
	api, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api,
		Credentials: &backendCredentials{developer: "credential-sentinel"}, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{}})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	gateway, registration := diagnosticGateway(t, backend, &fakeStore{}, nil, &logs)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // The fixture backend below still returns its actual independent validation failure.
	_, failure := api.GetIssue(context.Background(), "credential-sentinel", "acme", "widgets", 12)
	if failure == nil {
		t.Fatal("expected fixture failure")
	}
	request := rpcRequest(t, registration, mcp.ProtocolVersion, diagnosticCall)
	gateway.ServeHTTP(httptest.NewRecorder(), request)
	entries := diagnosticEntries(t, &logs)
	if len(entries) != 1 || entries[0]["failure_code"] != "github_invalid_response" || entries[0]["github_http_status"] != float64(200) || entries[0]["failure_reason"] != "invalid_state" || entries[0]["validation_field"] != "issue.state" {
		t.Fatalf("entries=%v", entries)
	}
	// A canceled request context is not evidence that this backend validation error was cancellation.
	logs.Reset()
	stub, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: &diagnosticGitHub{issueError: failure},
		Credentials: &backendCredentials{developer: "credential-sentinel"}, Publisher: &backendPublisher{}, Workflow: &backendWorkflow{}})
	if err != nil {
		t.Fatal(err)
	}
	gateway, registration = diagnosticGateway(t, stub, &fakeStore{}, nil, &logs)
	gateway.ServeHTTP(httptest.NewRecorder(), rpcRequest(t, registration, mcp.ProtocolVersion, diagnosticCall).WithContext(ctx))
	entries = diagnosticEntries(t, &logs)
	if len(entries) != 1 || entries[0]["failure_code"] != "github_invalid_response" {
		t.Fatalf("context replaced actual error: %v", entries)
	}
}

type diagnosticGitHub struct {
	backendGitHub
	issueID    int64
	issueError error
}

func (api *diagnosticGitHub) GetIssue(_ context.Context, credential, owner, repository string, number int) (githubapi.Issue, error) {
	api.record(mcp.ToolGetIssue, credential, owner, repository, number, "")
	return githubapi.Issue{ID: api.issueID, Number: number}, api.issueError
}

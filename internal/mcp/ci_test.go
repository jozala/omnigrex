package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/workflow"
)

const ciRegressionHead = "16d6f971a458044c1d6445171d20e0c3ef9b12fd"
const ciRegressionRun = 37244848238
const ciRegressionJob = 111560587662

func validScopeForCI() mcp.TokenScope {
	scope := validScope(time.Now())
	scope.HeadSHA = ciRegressionHead
	return scope
}

func ciRegressionLog() string {
	var builder strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&builder, "setup step %d: initializing docker container output line with filler text\n", i)
	}
	fmt.Fprint(&builder, "=== RUN   TestPRActivationDeferredReplaySettlesThroughReconciliation/first_subtest\n")
	fmt.Fprint(&builder, "    worker_test.go:123: CloseMutationAdmission() error = agent turn fence lost\n")
	fmt.Fprint(&builder, "--- FAIL: TestPRActivationDeferredReplaySettlesThroughReconciliation/first_subtest (0.01s)\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&builder, "intermediate log line %d: more container output\n", i)
	}
	fmt.Fprint(&builder, "=== RUN   TestPRActivationDeferredReplaySettlesThroughReconciliation/second_subtest\n")
	fmt.Fprint(&builder, "    worker_test.go:145: CloseMutationAdmission() error = agent turn fence lost\n")
	fmt.Fprint(&builder, "--- FAIL: TestPRActivationDeferredReplaySettlesThroughReconciliation/second_subtest (0.02s)\n")
	builder.WriteString(strings.Repeat("trailing output line\n", 200))
	return builder.String()
}

func ciRegressionServer(t *testing.T, logContent string) (*httptest.Server, *githubapi.APIClient) {
	t.Helper()
	var server *httptest.Server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/log-bytes" && r.Header.Get("Authorization") != "Bearer developer-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/repos/acme/widgets/commits/"+ciRegressionHead+"/check-runs":
			fmt.Fprintf(w, `{"total_count":1,"check_runs":[{"id":501,"node_id":"CR_501","name":"CI","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/runs/501","completed_at":"2026-10-06T10:00:00Z"}]}`, ciRegressionHead)
		case r.URL.Path == "/repos/acme/widgets/check-runs/501":
			fmt.Fprintf(w, `{"id":501,"node_id":"CR_501","name":"CI","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/runs/501","completed_at":"2026-10-06T10:00:00Z","output":{"title":"1 failing check","summary":"Docker-backed integration tests failed","text":"See annotations and logs"}}`, ciRegressionHead)
		case r.URL.Path == "/repos/acme/widgets/check-runs/501/annotations":
			fmt.Fprint(w, `[{"path":"internal/worker/worker_test.go","start_line":123,"end_line":123,"annotation_level":"failure","message":"CloseMutationAdmission() error = agent turn fence lost","title":"Test failure"}]`)
		case r.URL.Path == "/repos/acme/widgets/actions/runs" && r.URL.Query().Get("head_sha") == ciRegressionHead:
			fmt.Fprintf(w, `{"total_count":1,"workflow_runs":[{"id":%d,"name":"CI","head_branch":"omnigrex/issue-45","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/actions/runs/%d","run_number":10,"run_attempt":1}]}`, ciRegressionRun, ciRegressionHead, ciRegressionRun)
		case r.URL.Path == fmt.Sprintf("/repos/acme/widgets/actions/runs/%d", ciRegressionRun):
			fmt.Fprintf(w, `{"id":%d,"name":"CI","head_branch":"omnigrex/issue-45","head_sha":%q,"status":"completed","conclusion":"failure","html_url":"https://github.test/actions/runs/%d","run_number":10,"run_attempt":1}`, ciRegressionRun, ciRegressionHead, ciRegressionRun)
		case r.URL.Path == fmt.Sprintf("/repos/acme/widgets/actions/runs/%d/jobs", ciRegressionRun):
			fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":%d,"run_id":%d,"run_attempt":1,"head_sha":%q,"node_id":"J_1","name":"integration","status":"completed","conclusion":"failure","html_url":"https://github.test/actions/jobs/%d","started_at":"2026-10-06T10:00:00Z","completed_at":"2026-10-06T10:05:00Z","steps":[{"name":"Set up job","number":1,"status":"completed","conclusion":"success"},{"name":"Run Docker-backed integration tests","number":13,"status":"completed","conclusion":"failure"}]}]}`, ciRegressionJob, ciRegressionRun, ciRegressionHead, ciRegressionJob)
		case r.URL.Path == fmt.Sprintf("/repos/acme/widgets/actions/runs/%d/attempts/1/jobs", ciRegressionRun):
			fmt.Fprintf(w, `{"total_count":1,"jobs":[{"id":%d,"run_id":%d,"run_attempt":1,"head_sha":%q,"node_id":"J_1","name":"integration","status":"completed","conclusion":"failure","html_url":"https://github.test/actions/jobs/%d","started_at":"2026-10-06T10:00:00Z","completed_at":"2026-10-06T10:05:00Z","steps":[{"name":"Set up job","number":1,"status":"completed","conclusion":"success"},{"name":"Run Docker-backed integration tests","number":13,"status":"completed","conclusion":"failure"}]}]}`, ciRegressionJob, ciRegressionRun, ciRegressionHead, ciRegressionJob)
		case r.URL.Path == fmt.Sprintf("/repos/acme/widgets/actions/jobs/%d", ciRegressionJob):
			fmt.Fprintf(w, `{"id":%d,"run_id":%d,"run_attempt":1,"head_sha":%q,"node_id":"J_1","name":"integration","status":"completed","conclusion":"failure","html_url":"https://github.test/actions/jobs/%d","started_at":"2026-10-06T10:00:00Z","completed_at":"2026-10-06T10:05:00Z","steps":[{"name":"Set up job","number":1,"status":"completed","conclusion":"success"},{"name":"Run Docker-backed integration tests","number":13,"status":"completed","conclusion":"failure"}]}`, ciRegressionJob, ciRegressionRun, ciRegressionHead, ciRegressionJob)
		case r.URL.Path == fmt.Sprintf("/repos/acme/widgets/actions/jobs/%d/logs", ciRegressionJob):
			w.Header().Set("Location", server.URL+"/log-bytes")
			w.WriteHeader(http.StatusFound)
		case r.URL.Path == "/log-bytes":
			if r.Header.Get("Authorization") != "" {
				http.Error(w, "auth forwarded", http.StatusBadRequest)
				return
			}
			if rng := r.Header.Get("Range"); rng != "" {
				var start int64
				if _, err := fmt.Sscanf(rng, "bytes=%d-", &start); err == nil && start < int64(len(logContent)) {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(logContent)-1, len(logContent)))
					w.WriteHeader(http.StatusPartialContent)
					_, _ = io.WriteString(w, logContent[start:])
					return
				}
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			_, _ = io.WriteString(w, logContent)
		default:
			http.NotFound(w, r)
		}
	})
	server = httptest.NewServer(handler)
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return server, client
}

func ciBackend(t *testing.T, client *githubapi.APIClient, role workflow.Role) (*mcp.ProductionBackend, mcp.ToolScope) {
	t.Helper()
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: client, Credentials: &backendCredentials{developer: "developer-secret", reviewer: "reviewer-secret"},
		Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := productionToolScope(role)
	scope.Repository = mcp.RepositoryScope{ID: 9123, Owner: "acme", Name: "widgets"}
	scope.HeadSHA = ciRegressionHead
	return backend, scope
}

func ciExecute(t *testing.T, backend *mcp.ProductionBackend, scope mcp.ToolScope, name, arguments string) json.RawMessage {
	t.Helper()
	result, err := backend.Execute(context.Background(), mcp.Invocation{Name: name, Arguments: json.RawMessage(arguments), Scope: scope, Class: mcp.ReadTool})
	if err != nil {
		t.Fatalf("Execute(%s) error = %v", name, err)
	}
	if !json.Valid(result) {
		t.Fatalf("Execute(%s) invalid JSON: %s", name, result)
	}
	return result
}

func TestCIRegressionBothRolesRetrieveFailedStepAndAssertion(t *testing.T) {
	logContent := ciRegressionLog()
	server, client := ciRegressionServer(t, logContent)
	defer server.Close()
	for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		t.Run(string(role), func(t *testing.T) {
			backend, scope := ciBackend(t, client, role)
			checksRaw := ciExecute(t, backend, scope, mcp.ToolGetCheckRuns, `{}`)
			var checks []map[string]any
			if err := json.Unmarshal(checksRaw, &checks); err != nil || len(checks) != 1 {
				t.Fatalf("get_check_runs = %s, %v", checksRaw, err)
			}
			diagRaw := ciExecute(t, backend, scope, mcp.ToolGetCheckRunDiagnostics, `{"check_run_id":501}`)
			if !strings.Contains(string(diagRaw), "Docker-backed integration tests failed") {
				t.Fatalf("diagnostics missing summary: %s", diagRaw)
			}
			runsRaw := ciExecute(t, backend, scope, mcp.ToolListCIRuns, `{}`)
			if !strings.Contains(string(runsRaw), "37244848238") {
				t.Fatalf("runs missing run: %s", runsRaw)
			}
			runRaw := ciExecute(t, backend, scope, mcp.ToolGetCIRun, fmt.Sprintf(`{"run_id":%d,"attempt":1}`, ciRegressionRun))
			var runDetail struct {
				Jobs []struct {
					ID    int64 `json:"job_id"`
					Steps []struct {
						Name       string `json:"name"`
						Number     int    `json:"number"`
						Conclusion string `json:"conclusion"`
					} `json:"steps"`
				} `json:"jobs"`
			}
			if err := json.Unmarshal(runRaw, &runDetail); err != nil || len(runDetail.Jobs) != 1 {
				t.Fatalf("get_ci_run = %s, %v", runRaw, err)
			}
			foundStep := false
			for _, step := range runDetail.Jobs[0].Steps {
				if step.Number == 13 && step.Name == "Run Docker-backed integration tests" && step.Conclusion == "failure" {
					foundStep = true
				}
			}
			if !foundStep {
				t.Fatalf("failed step 13 not found: %s", runRaw)
			}
			searchRaw := ciExecute(t, backend, scope, mcp.ToolSearchCIJobLogs, fmt.Sprintf(`{"job_id":%d,"query":"agent turn fence lost","context_lines":3}`, ciRegressionJob))
			if !strings.Contains(string(searchRaw), "agent turn fence lost") {
				t.Fatalf("search missing assertion: %s", searchRaw)
			}
			firstRaw := ciExecute(t, backend, scope, mcp.ToolGetCIJobLogs, fmt.Sprintf(`{"job_id":%d,"max_bytes":4096}`, ciRegressionJob))
			var first struct {
				Text       string `json:"text"`
				HasMore    bool   `json:"has_more"`
				NextCursor string `json:"next_cursor"`
			}
			if err := json.Unmarshal(firstRaw, &first); err != nil || !first.HasMore || first.NextCursor == "" {
				t.Fatalf("first excerpt missing continuation: %s, %v", firstRaw, err)
			}
			if strings.Contains(first.Text, "agent turn fence lost") {
				t.Fatal("assertion should be beyond first excerpt window")
			}
			combined := first.Text
			cursor := first.NextCursor
			for i := 0; i < 20 && cursor != ""; i++ {
				nextRaw := ciExecute(t, backend, scope, mcp.ToolGetCIJobLogs, fmt.Sprintf(`{"job_id":%d,"cursor":%q,"max_bytes":16384}`, ciRegressionJob, cursor))
				var next struct {
					Text       string `json:"text"`
					HasMore    bool   `json:"has_more"`
					NextCursor string `json:"next_cursor"`
				}
				if err := json.Unmarshal(nextRaw, &next); err != nil {
					t.Fatalf("continuation %d error: %v", i, err)
				}
				combined += next.Text
				cursor = next.NextCursor
				if !next.HasMore {
					break
				}
			}
			if !strings.Contains(combined, "TestPRActivationDeferredReplaySettlesThroughReconciliation") || !strings.Contains(combined, "CloseMutationAdmission() error = agent turn fence lost") {
				t.Fatal("combined excerpts missing both subtests and assertion")
			}
			if strings.Count(combined, "agent turn fence lost") < 2 {
				t.Fatalf("expected both subtest assertions, got %d", strings.Count(combined, "agent turn fence lost"))
			}
		})
	}
}

func TestCIScopeRejectionForgedAndCrossHead(t *testing.T) {
	server, client := ciRegressionServer(t, ciRegressionLog())
	defer server.Close()
	backend, scope := ciBackend(t, client, workflow.RoleDeveloper)
	for _, test := range []struct {
		name, arguments string
	}{
		{"forged check", `{"check_run_id":999999}`},
		{"forged run", `{"run_id":999999}`},
		{"forged job logs", `{"job_id":999999}`},
		{"forged search", `{"job_id":999999,"query":"x"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var tool string
			switch {
			case strings.Contains(test.name, "check"):
				tool = mcp.ToolGetCheckRunDiagnostics
			case strings.Contains(test.name, "run") && !strings.Contains(test.name, "job"):
				tool = mcp.ToolGetCIRun
			case strings.Contains(test.name, "search"):
				tool = mcp.ToolSearchCIJobLogs
			default:
				tool = mcp.ToolGetCIJobLogs
			}
			_, err := backend.Execute(context.Background(), mcp.Invocation{Name: tool, Arguments: json.RawMessage(test.arguments), Scope: scope, Class: mcp.ReadTool})
			if err == nil || !strings.Contains(err.Error(), "ci diagnostics ") {
				t.Fatalf("forged call error = %v", err)
			}
			if !strings.Contains(err.Error(), "missing") && !strings.Contains(err.Error(), "scope_rejected") {
				t.Fatalf("forged call not distinguishable: %v", err)
			}
		})
	}
	crossHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/widgets/check-runs/501" {
			fmt.Fprint(w, `{"id":501,"node_id":"CR_501","name":"CI","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","status":"completed","conclusion":"failure","html_url":"https://github.test/x","completed_at":"2026-10-06T10:00:00Z","output":{}}`)
			return
		}
		if r.URL.Path == "/repos/acme/widgets/check-runs/501/annotations" {
			fmt.Fprint(w, `[]`)
			return
		}
		http.NotFound(w, r)
	})
	crossServer := httptest.NewServer(crossHandler)
	defer crossServer.Close()
	crossClient, err := githubapi.NewAPIClient(crossServer.Client(), crossServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	crossBackend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: crossClient, Credentials: &backendCredentials{developer: "developer-secret"},
		Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = crossBackend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolGetCheckRunDiagnostics, Arguments: json.RawMessage(`{"check_run_id":501}`), Scope: scope, Class: mcp.ReadTool})
	if err == nil || !strings.Contains(err.Error(), "scope_rejected") {
		t.Fatalf("cross-head error = %v", err)
	}
	_, err = backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolGetCIJobLogs, Arguments: json.RawMessage(fmt.Sprintf(`{"job_id":%d,"step_number":99}`, ciRegressionJob)), Scope: scope, Class: mcp.ReadTool})
	if err == nil || !strings.Contains(err.Error(), "scope_rejected") {
		t.Fatalf("invalid step error = %v", err)
	}
	_, err = backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolGetCIJobLogs, Arguments: json.RawMessage(fmt.Sprintf(`{"job_id":%d,"cursor":"bad"}`, ciRegressionJob)), Scope: scope, Class: mcp.ReadTool})
	if err == nil || !strings.Contains(err.Error(), "invalid_continuation") {
		t.Fatalf("invalid cursor error = %v", err)
	}
}

func TestCIDiagnosticStatesAndPermissionsAreDistinguishable(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-GitHub-Request-Id", "A41A:2AF0FB:6AAAD0:6A71D9:6ABC2962")
		http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := githubapi.NewAPIClient(server.Client(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: client, Credentials: &backendCredentials{developer: "developer-secret"},
		Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := productionToolScope(workflow.RoleDeveloper)
	scope.HeadSHA = ciRegressionHead
	_, err = backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolListCIRuns, Arguments: json.RawMessage(`{}`), Scope: scope, Class: mcp.ReadTool})
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("permission error = %v", err)
	}
}

func TestCIGatewayExposesDistinctErrorsWithoutRawLogs(t *testing.T) {
	logContent := "secret-assertion-line: agent turn fence lost\nsigned-url-sentinel https://example.blob.core.windows.net/sig\n"
	server, client := ciRegressionServer(t, logContent)
	defer server.Close()
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub: client, Credentials: &backendCredentials{developer: "developer-secret"},
		Publisher: &backendPublisher{}, Workflow: &backendWorkflow{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: &fakeStore{}, Backend: backend,
		Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := validScopeForCI()
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatal(err)
	}
	initialize(t, gateway, registration)
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_ci_job_logs","arguments":{"job_id":999999}}}`))
	body := response.Body.String()
	if !strings.Contains(body, "missing") && !strings.Contains(body, "scope_rejected") {
		t.Fatalf("gateway missing response = %s", body)
	}
	if strings.Contains(body, "secret-assertion-line") || strings.Contains(body, "signed-url-sentinel") || strings.Contains(body, "developer-secret") {
		t.Fatalf("gateway disclosed raw diagnostics: %s", body)
	}
	for _, secret := range []string{"secret-assertion-line", "signed-url-sentinel", "developer-secret", "blob.core.windows.net/sig"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("operational logs disclosed %s: %s", secret, logs.String())
		}
	}
}

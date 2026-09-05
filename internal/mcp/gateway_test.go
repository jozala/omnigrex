package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestDeveloperListsOnlyItsFixedConcreteToolSetAfterInitialization(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{}
	gateway := newTestGateway(t, now, durable, fakeBackend{})
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	if response.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	want := []string{
		"get_issue", "list_issue_comments", "get_pull_request", "list_pull_request_reviews", "list_review_threads", "get_check_runs",
		"publish_changes", "open_pr", "request_review", "comment_on_issue", "comment_on_pull_request", "report_blocked",
	}
	got := make([]string, len(payload.Result.Tools))
	for index, tool := range payload.Result.Tools {
		got[index] = tool.Name
		if tool.Description == "" || tool.InputSchema["type"] != "object" || tool.InputSchema["additionalProperties"] != false {
			t.Errorf("tool %q has non-concrete metadata: %#v", tool.Name, tool)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Developer tools = %v, want %v", got, want)
	}
	if durable.validationCount() != 3 {
		t.Fatalf("ValidateTurnFence calls = %d, want 3", durable.validationCount())
	}
}

func TestReadToolCallIsFencedScopedAndRecorded(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{}
	backend := &recordingBackend{result: json.RawMessage(`{"number":12,"title":"Fix it"}`)}
	ledger := &recordingLedger{}
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend, Ledger: ledger,
		Now: func() time.Time { return now }, Random: bytes.NewReader(bytes.Repeat([]byte{0x31}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":"read-1","method":"tools/call","params":{"name":"get_issue","arguments":{}}}`))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `\"number\":12`) {
		t.Fatalf("tools/call response = %d %s", response.Code, response.Body.String())
	}
	call := backend.singleCall(t)
	if call.Name != mcp.ToolGetIssue || call.Class != mcp.ReadTool || call.Scope.WorkflowID != "workflow-1" ||
		call.Scope.AgentSessionID != "session-1" || call.Scope.ExecutionEpoch != 4 || call.Scope.Repository.ID != 9123 || call.Scope.Issue.Number != 12 {
		t.Fatalf("backend invocation = %#v", call)
	}
	if durable.validationCount() != 3 || ledger.count() != 1 || !ledger.records[0].Succeeded || string(ledger.records[0].Result) != `{"number":12,"title":"Fix it"}` {
		t.Fatalf("fence calls = %d, read records = %#v", durable.validationCount(), ledger.records)
	}
}

func TestReadToolDoesNotReturnDataWhenLedgerRecordingFails(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	backend := &recordingBackend{result: json.RawMessage(`{"number":12,"secret":"dependency-secret"}`)}
	ledger := &recordingLedger{err: errors.New("database-secret write failed")}
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: &fakeStore{}, Backend: backend, Ledger: ledger,
		Now: func() time.Time { return now }, Random: bytes.NewReader(bytes.Repeat([]byte{0x32}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":"read-ledger-failure","method":"tools/call","params":{"name":"get_issue","arguments":{}}}`))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"isError":true`) {
		t.Fatalf("tools/call response = %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "dependency-secret") || strings.Contains(response.Body.String(), "database-secret") {
		t.Fatalf("tools/call disclosed dependency data: %s", response.Body.String())
	}
	if backend.count() != 1 || ledger.count() != 1 {
		t.Fatalf("backend calls = %d, ledger calls = %d", backend.count(), ledger.count())
	}
}

func TestReviewerCannotCallDeveloperMutation(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{}
	backend := &recordingBackend{}
	gateway := newTestGateway(t, now, durable, backend)
	scope := validScope(now)
	scope.Role = workflow.RoleReviewer
	scope.Lease.AgentProfileConfig = json.RawMessage(`{"role":"REVIEWER"}`)
	scope.Lease.ChangeProposalID = "proposal-1"
	scope.Lease.ExpectedHeadSHA = scope.HeadSHA
	scope.PullRequest = &mcp.PullRequestScope{ID: 654, Number: 23}
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"publish_changes","arguments":{"operation_id":"publish-1","message":"no"}}}`))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"code":-32602`) {
		t.Fatalf("forbidden tools/call response = %d %s", response.Code, response.Body.String())
	}
	if durable.validationCount() != 3 || backend.count() != 0 {
		t.Fatalf("forbidden call reached dependencies: fence = %d, backend = %d", durable.validationCount(), backend.count())
	}
}

func TestMutationIsDurablySequencedAndSuccessfulRetryUsesCachedResult(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{}
	backend := &recordingBackend{result: json.RawMessage(`{"comment_id":42}`)}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)
	body := `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"issue-comment-42","body":"Implemented"}}}`

	first := httptest.NewRecorder()
	gateway.ServeHTTP(first, rpcRequest(t, registration, mcp.ProtocolVersion, body))
	second := httptest.NewRecorder()
	gateway.ServeHTTP(second, rpcRequest(t, registration, mcp.ProtocolVersion, body))
	if first.Code != http.StatusOK || second.Code != http.StatusOK || first.Body.String() != second.Body.String() || !strings.Contains(first.Body.String(), `\"comment_id\":42`) {
		t.Fatalf("mutation responses = (%d %s) and (%d %s)", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
	if backend.count() != 1 {
		t.Fatalf("backend calls = %d, want one", backend.count())
	}
	call := backend.singleCall(t)
	if call.Class != mcp.MutationTool || call.OperationID != testMutationID(1) || call.Mutation.ExternalService != "github" || call.Mutation.ExternalResourceID != "9123:456" {
		t.Fatalf("mutation invocation metadata = %#v", call)
	}
	if strings.Contains(call.OperationID, "issue-comment-42") {
		t.Fatalf("artifact operation identity %q contains caller-controlled idempotency key", call.OperationID)
	}
	reserved, started, completed, failed, unknown := durable.mutationCounts()
	if reserved != 2 || started != 1 || completed != 1 || failed != 0 || unknown != 0 {
		t.Fatalf("mutation lifecycle counts = reserve %d, start %d, complete %d, fail %d, unknown %d", reserved, started, completed, failed, unknown)
	}
}

func TestSameTurnPublishAndOpenPullRequestReservationsUseLatestPublishedHead(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	firstHead := "1123456789abcdef0123456789abcdef01234567"
	secondHead := "2123456789abcdef0123456789abcdef01234567"
	durable := &fakeStore{}
	api := &backendGitHub{openHead: secondHead}
	publisher := &backendPublisher{results: []workspace.PublicationResult{
		{Head: firstHead, Changed: true},
		{Head: secondHead, Changed: true},
	}}
	production, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
		GitHub:      api,
		Credentials: &backendCredentials{developer: "developer-secret"},
		Publisher:   publisher,
		Workflow:    &backendWorkflow{requestReviewResult: json.RawMessage(`{"handoff_id":"review-handoff"}`)},
	})
	if err != nil {
		t.Fatalf("NewProductionBackend() error = %v", err)
	}
	gateway := newTestGateway(t, now, durable, production)
	scope := validScope(now)
	scope.HeadSHA = productionHeadSHA
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	requests := []string{
		`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"publish_changes","arguments":{"operation_id":"publish-1","message":"First"}}}`,
		`{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"publish_changes","arguments":{"operation_id":"publish-2","message":"Second"}}}`,
		`{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"publish_changes","arguments":{"operation_id":"publish-1","message":"First"}}}`,
		`{"jsonrpc":"2.0","id":23,"method":"tools/call","params":{"name":"open_pr","arguments":{"operation_id":"open-1","title":"Changes","body":"Ready"}}}`,
		`{"jsonrpc":"2.0","id":24,"method":"tools/call","params":{"name":"request_review","arguments":{"operation_id":"request-review-1","summary":"Ready"}}}`,
	}
	for _, body := range requests {
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, body))
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"isError":true`) {
			t.Fatalf("mutation response = %d %s", response.Code, response.Body.String())
		}
	}

	specs := durable.mutationSpecs()
	if len(specs) != 5 {
		t.Fatalf("mutation reservations = %#v", specs)
	}
	wantHeads := []string{productionHeadSHA, firstHead, productionHeadSHA, secondHead, secondHead}
	for index, want := range wantHeads {
		if specs[index].ExpectedSHA != want {
			t.Errorf("reservation %d ExpectedSHA = %q, want %q", index+1, specs[index].ExpectedSHA, want)
		}
	}
	if specs[4].ExternalService != "omnigrex" || specs[4].ExternalResourceID != "9123:omnigrex/issue-12" {
		t.Fatalf("request_review reservation metadata = %#v", specs[4])
	}
	if specs[3].ExternalService != "github" || specs[3].ExternalResourceID != "9123:omnigrex/issue-12:main" {
		t.Fatalf("open_pr reservation metadata = %#v", specs[3])
	}
	wantMarker, err := githubapi.RenderMarker(githubapi.Marker{WorkflowID: "workflow-1", AgentAssignmentID: "assignment-1", OperationID: testMutationID(3)})
	if err != nil {
		t.Fatal(err)
	}
	if api.openRequest.Marker != wantMarker || strings.Contains(api.openRequest.Marker, "open-1") {
		t.Fatalf("open Pull Request marker = %q, want reservation identity only", api.openRequest.Marker)
	}
	if len(publisher.publications) != 2 || strings.Contains(publisher.publications[0].Message, "publish-1") ||
		!strings.Contains(publisher.publications[0].Message, "Omnigrex-Operation-ID: "+testMutationID(1)) {
		t.Fatalf("publication operation trailers = %#v", publisher.publications)
	}
}

func TestRevocationImmediatelyRejectsTheBearerWithoutDisclosingIt(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	gateway := newTestGateway(t, now, &fakeStore{}, fakeBackend{})
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	bearer := registration.Server.Headers[0].Value
	if !gateway.Revoke(registration) {
		t.Fatal("Revoke() = false, want true")
	}

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), bearer) {
		t.Fatalf("revoked response = %d %q", response.Code, response.Body.String())
	}
	if gateway.Revoke(registration) {
		t.Fatal("second Revoke() = true, want false")
	}
}

func TestRevocationReleasesEachGrantExactlyOnceAfterAdmittedCallsDrain(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	backend := &releasingBackend{
		started: make(chan struct{}), executeRelease: make(chan struct{}), released: make(chan mcp.ToolScope, 1),
	}
	gateway := newTestGateway(t, now, &fakeStore{}, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	handlerReturned := make(chan struct{})
	go func() {
		body := `{"jsonrpc":"2.0","id":25,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"release-after-drain","body":"Wait"}}}`
		gateway.ServeHTTP(httptest.NewRecorder(), rpcRequest(t, registration, mcp.ProtocolVersion, body))
		close(handlerReturned)
	}()
	<-backend.started
	if !gateway.Revoke(registration) {
		t.Fatal("Revoke() = false, want true")
	}
	select {
	case scope := <-backend.released:
		t.Fatalf("ReleaseTurn(%#v) called before admitted mutation drained", scope)
	case <-time.After(50 * time.Millisecond):
	}

	close(backend.executeRelease)
	select {
	case scope := <-backend.released:
		if scope.AgentAssignmentID != "assignment-1" || scope.AgentTurnID != "turn-1" || scope.ExecutionEpoch != 4 {
			t.Fatalf("ReleaseTurn scope = %#v", scope)
		}
	case <-time.After(time.Second):
		t.Fatal("ReleaseTurn was not called after drain")
	}
	<-handlerReturned
	for range 2 {
		if err := gateway.CloseAndDrain(context.Background(), registration); err != nil {
			t.Fatalf("CloseAndDrain() retry error = %v", err)
		}
	}
	if gateway.Revoke(registration) {
		t.Fatal("Revoke() after release = true, want false")
	}
	if got := backend.releaseCount(); got != 1 {
		t.Fatalf("ReleaseTurn calls = %d, want 1", got)
	}
}

func TestOverlappingSameSessionRegistrationsStaySerializedAndRetireTheirGate(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{}
	backend := &serialBackend{entered: make(chan string, 2), releaseFirst: make(chan struct{})}
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	first, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("first Register() error = %v", err)
	}
	secondScope := validScope(now)
	secondScope.Lease.ID = "turn-2"
	secondScope.Lease.JobLease.AgentTurnID = "turn-2"
	secondScope.Lease.ExecutionEpoch = 5
	secondScope.Lease.JobLease.ExecutionEpoch = 5
	second, err := gateway.Register(secondScope)
	if err != nil {
		t.Fatalf("second Register() error = %v", err)
	}
	initialize(t, gateway, first)
	initialize(t, gateway, second)

	call := func(registration mcp.Registration, operationID string, done chan<- struct{}) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":26,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":%q,"body":"comment"}}}`, operationID)
		gateway.ServeHTTP(httptest.NewRecorder(), rpcRequest(t, registration, mcp.ProtocolVersion, body))
		done <- struct{}{}
	}
	done := make(chan struct{}, 2)
	go call(first, "first-registration", done)
	if operationID := <-backend.entered; operationID != testMutationID(1) {
		t.Fatalf("first backend operation = %q", operationID)
	}
	go call(second, "second-registration", done)
	select {
	case operationID := <-backend.entered:
		t.Fatalf("operation %q entered while first registration was active", operationID)
	case <-time.After(50 * time.Millisecond):
	}

	firstDrained := make(chan error, 1)
	go func() { firstDrained <- gateway.CloseAndDrain(context.Background(), first) }()
	close(backend.releaseFirst)
	if operationID := <-backend.entered; operationID != testMutationID(2) {
		t.Fatalf("second backend operation = %q", operationID)
	}
	if err := <-firstDrained; err != nil {
		t.Fatalf("first CloseAndDrain() error = %v", err)
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("serialized mutation did not finish")
		}
	}
	if err := gateway.CloseAndDrain(context.Background(), second); err != nil {
		t.Fatalf("second CloseAndDrain() error = %v", err)
	}
	if got := reflect.ValueOf(gateway).Elem().FieldByName("gates").Len(); got != 0 {
		t.Fatalf("retained session gates = %d, want 0", got)
	}
}

func TestRegistrationChurnReturnsGatewayStateToBaseline(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	const registrationCount = 100
	backend := &releasingBackend{released: make(chan mcp.ToolScope, registrationCount)}
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: &fakeStore{}, Backend: backend,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for index := 0; index < registrationCount; index++ {
		scope := validScope(now)
		scope.Lease.ID = fmt.Sprintf("turn-%d", index)
		scope.Lease.JobLease.AgentTurnID = scope.Lease.ID
		scope.Lease.ExecutionEpoch = int64(index + 1)
		scope.Lease.JobLease.ExecutionEpoch = scope.Lease.ExecutionEpoch
		registration, err := gateway.Register(scope)
		if err != nil {
			t.Fatalf("Register(%d) error = %v", index, err)
		}
		if !gateway.Revoke(registration) {
			t.Fatalf("Revoke(%d) = false", index)
		}
	}
	value := reflect.ValueOf(gateway).Elem()
	if registrations := value.FieldByName("registrations").Len(); registrations != 0 {
		t.Fatalf("retained registrations = %d, want 0", registrations)
	}
	if gates := value.FieldByName("gates").Len(); gates != 0 {
		t.Fatalf("retained session gates = %d, want 0", gates)
	}
	if got := backend.releaseCount(); got != registrationCount {
		t.Fatalf("ReleaseTurn calls = %d, want %d", got, registrationCount)
	}
}

func TestAmbiguousBackendFailureIsMarkedUnknownWithoutExposingItsCause(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{}
	backend := &recordingBackend{err: mcp.OutcomeUnknown(errors.New("upstream timeout with bearer super-secret"))}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"report_blocked","arguments":{"operation_id":"blocked-1","reason":"Cannot proceed"}}}`))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"isError":true`) || strings.Contains(response.Body.String(), "super-secret") {
		t.Fatalf("ambiguous mutation response = %d %s", response.Code, response.Body.String())
	}
	_, _, completed, failed, unknown := durable.mutationCounts()
	if completed != 0 || failed != 0 || unknown != 1 {
		t.Fatalf("ambiguous mutation states = complete %d, fail %d, unknown %d", completed, failed, unknown)
	}
	if err := gateway.CloseAndDrain(context.Background(), registration); err != nil {
		t.Fatalf("CloseAndDrain() error = %v for durable UNKNOWN mutation", err)
	}
}

func TestAmbiguousReservationFailureDrainsUnresolvedWithoutStartingBackend(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{reserveErr: errors.New("commit acknowledgement database-secret")}
	backend := &recordingBackend{result: json.RawMessage(`{"comment_id":42}`)}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":27,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"ambiguous-reservation","body":"comment"}}}`))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"isError":true`) || strings.Contains(response.Body.String(), "database-secret") {
		t.Fatalf("ambiguous reservation response = %d %s", response.Code, response.Body.String())
	}
	if backend.count() != 0 {
		t.Fatalf("backend calls = %d, want zero", backend.count())
	}
	if state := durable.mutationState("ambiguous-reservation"); state != store.MutationReserved {
		t.Fatalf("committed mutation state = %s, want %s", state, store.MutationReserved)
	}
	assertUnresolvedDrain(t, gateway, registration)
}

func TestAdmittedMutationContinuesAfterHTTPRequestCancellation(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{completionObserved: make(chan struct{}, 1)}
	backend := &blockingBackend{started: make(chan struct{}), release: make(chan struct{}), contextCanceled: make(chan bool, 1)}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	ctx, cancel := context.WithCancel(context.Background())
	request := rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"detached-1","body":"Still execute"}}}`).WithContext(ctx)
	handlerReturned := make(chan struct{})
	go func() {
		gateway.ServeHTTP(httptest.NewRecorder(), request)
		close(handlerReturned)
	}()
	<-backend.started
	cancel()
	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		t.Fatal("HTTP handler did not stop waiting after cancellation")
	}
	close(backend.release)
	select {
	case canceled := <-backend.contextCanceled:
		if canceled {
			t.Fatal("admitted backend operation inherited HTTP cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("backend operation did not finish")
	}
	select {
	case <-durable.completionObserved:
	case <-time.After(time.Second):
		t.Fatal("admitted mutation was not completed durably")
	}
}

func TestMutationFenceLossCancelsBackendAndDrainsAsUnresolved(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{unknownErr: errors.New("unknown database-secret")}
	backend := &cancelingBackend{started: make(chan struct{}), finished: make(chan struct{})}
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend,
		MutationFenceCheckInterval: 100 * time.Microsecond,
		Now:                        func() time.Time { return now },
		Random:                     bytes.NewReader(bytes.Repeat([]byte{0x35}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	scope := validScope(now)
	registration, err := gateway.Register(scope)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	handlerReturned := make(chan struct{})
	go func() {
		body := `{"jsonrpc":"2.0","id":20,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"fence-loss","body":"Wait"}}}`
		gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, body))
		close(handlerReturned)
	}()
	<-backend.started
	durable.setValidateError(store.ErrAgentTurnFenceLost)

	select {
	case <-backend.finished:
	case <-time.After(time.Second):
		t.Fatal("lease loss did not cancel the active backend operation")
	}
	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		t.Fatal("fence-canceled mutation did not finish durable handling")
	}
	if !strings.Contains(response.Body.String(), "mutation durable state is unresolved") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("fence-canceled mutation response = %d %s", response.Code, response.Body.String())
	}
	validations := durable.validationLeasesSnapshot()
	if len(validations) < 5 || !reflect.DeepEqual(validations[len(validations)-1], scope.Lease) {
		t.Fatal("watcher did not validate the exact registered lease")
	}
	_, _, _, _, unknown := durable.mutationCounts()
	if unknown != 1 {
		t.Fatalf("MarkMutationUnknown calls = %d, want one", unknown)
	}
	assertUnresolvedDrain(t, gateway, registration)
}

func TestMutationLifecycleCancellationFinalizesUnknownAndDrains(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	lifecycleValue := &struct{ name string }{"lifecycle-value"}
	lifecycle, cancelLifecycle := context.WithCancel(context.WithValue(context.Background(), lifecycleContextKey{}, lifecycleValue))
	durable := &fakeStore{unknownObserved: make(chan mutationContextObservation, 1)}
	backend := &cancelingBackend{started: make(chan struct{}), finished: make(chan struct{})}
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend,
		LifecycleContext: lifecycle, MutationFenceCheckInterval: time.Hour, MutationFinalizationTimeout: time.Second,
		Now: func() time.Time { return now }, Random: bytes.NewReader(bytes.Repeat([]byte{0x36}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	handlerReturned := make(chan struct{})
	go func() {
		body := `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"lifecycle-cancel","body":"Wait"}}}`
		gateway.ServeHTTP(httptest.NewRecorder(), rpcRequest(t, registration, mcp.ProtocolVersion, body))
		close(handlerReturned)
	}()
	<-backend.started
	cancelLifecycle()
	select {
	case <-backend.finished:
	case <-time.After(time.Second):
		t.Fatal("lifecycle cancellation did not cancel the active backend operation")
	}
	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		t.Fatal("lifecycle-canceled mutation did not finish durable handling")
	}
	observation := <-durable.unknownObserved
	if observation.contextErr != nil || observation.value != lifecycleValue || !observation.hasDeadline {
		t.Fatalf("UNKNOWN finalization context = error %v, value %#v, deadline %t", observation.contextErr, observation.value, observation.hasDeadline)
	}
	_, _, _, _, unknown := durable.mutationCounts()
	if unknown != 1 || durable.mutationState("lifecycle-cancel") != store.MutationUnknown {
		t.Fatalf("UNKNOWN transition = calls %d, state %s", unknown, durable.mutationState("lifecycle-cancel"))
	}
	if err := gateway.CloseAndDrain(context.Background(), registration); err != nil {
		t.Fatalf("CloseAndDrain() error = %v", err)
	}
}

func TestMutationOperationDeadlineCancelsBlockedBackendAndPersistsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{unknownObserved: make(chan mutationContextObservation, 1)}
	backend := &cancelingBackend{started: make(chan struct{}), finished: make(chan struct{})}
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend,
		MutationOperationTimeout: 50 * time.Millisecond, MutationFenceCheckInterval: time.Hour, MutationFinalizationTimeout: time.Second,
		Now: func() time.Time { return now }, Random: bytes.NewReader(bytes.Repeat([]byte{0x38}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	handlerReturned := make(chan struct{})
	go func() {
		body := `{"jsonrpc":"2.0","id":28,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"operation-deadline","body":"Wait"}}}`
		gateway.ServeHTTP(httptest.NewRecorder(), rpcRequest(t, registration, mcp.ProtocolVersion, body))
		close(handlerReturned)
	}()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("backend operation did not start")
	}
	select {
	case <-backend.finished:
	case <-time.After(time.Second):
		t.Fatal("operation deadline did not cancel the backend")
	}
	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		t.Fatal("deadline-canceled mutation did not finish")
	}
	observation := <-durable.unknownObserved
	if observation.contextErr != nil || !observation.hasDeadline {
		t.Fatalf("UNKNOWN finalization context = error %v, deadline %t", observation.contextErr, observation.hasDeadline)
	}
	if state := durable.mutationState("operation-deadline"); state != store.MutationUnknown {
		t.Fatalf("deadline-canceled mutation state = %s, want %s", state, store.MutationUnknown)
	}
	if err := gateway.CloseAndDrain(context.Background(), registration); err != nil {
		t.Fatalf("CloseAndDrain() error = %v", err)
	}
}

func TestCloseAndDrainCancelsBlockedMutationAndPersistsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{unknownObserved: make(chan mutationContextObservation, 1)}
	backend := &cancelingBackend{started: make(chan struct{}), finished: make(chan struct{})}
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend,
		MutationOperationTimeout: time.Hour, MutationFenceCheckInterval: time.Hour, MutationFinalizationTimeout: time.Second,
		Now: func() time.Time { return now }, Random: bytes.NewReader(bytes.Repeat([]byte{0x39}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	handlerReturned := make(chan struct{})
	go func() {
		body := `{"jsonrpc":"2.0","id":29,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"drain-cancel","body":"Wait"}}}`
		gateway.ServeHTTP(httptest.NewRecorder(), rpcRequest(t, registration, mcp.ProtocolVersion, body))
		close(handlerReturned)
	}()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("backend operation did not start")
	}

	drainReturned := make(chan error, 1)
	go func() { drainReturned <- gateway.CloseAndDrain(context.Background(), registration) }()
	select {
	case err := <-drainReturned:
		if err != nil {
			t.Fatalf("CloseAndDrain() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CloseAndDrain() did not cancel and drain the blocked mutation")
	}
	select {
	case <-backend.finished:
	default:
		t.Fatal("CloseAndDrain() returned before the backend observed cancellation")
	}
	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		t.Fatal("drain-canceled mutation handler did not finish")
	}
	observation := <-durable.unknownObserved
	if observation.contextErr != nil || !observation.hasDeadline {
		t.Fatalf("UNKNOWN finalization context = error %v, deadline %t", observation.contextErr, observation.hasDeadline)
	}
	if state := durable.mutationState("drain-cancel"); state != store.MutationUnknown {
		t.Fatalf("drain-canceled mutation state = %s, want %s", state, store.MutationUnknown)
	}
}

func TestSuccessfulShortMutationStopsFenceWatcher(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{}
	backend := &recordingBackend{result: json.RawMessage(`{"comment_id":42}`)}
	const interval = 50 * time.Millisecond
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: backend,
		MutationFenceCheckInterval: interval,
		Now:                        func() time.Time { return now },
		Random:                     bytes.NewReader(bytes.Repeat([]byte{0x37}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	body := `{"jsonrpc":"2.0","id":22,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"short-success","body":"Done"}}}`
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, body))
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"isError":true`) {
		t.Fatalf("mutation response = %d %s", response.Code, response.Body.String())
	}
	if got := durable.validationCount(); got != 4 {
		t.Fatalf("ValidateTurnFence calls at completion = %d, want 4", got)
	}
	time.Sleep(2 * interval)
	if got := durable.validationCount(); got != 4 {
		t.Fatalf("stopped watcher made later ValidateTurnFence calls: %d", got)
	}
}

func TestCloseAndDrainWaitsForNonCooperativeMutationAfterHTTPRequestCancellation(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{unknownObserved: make(chan mutationContextObservation, 1)}
	backend := &blockingBackend{started: make(chan struct{}), release: make(chan struct{}), contextCanceled: make(chan bool, 1)}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	requestContext, cancelRequest := context.WithCancel(context.Background())
	request := rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":15,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"drain-detached","body":"Still execute"}}}`).WithContext(requestContext)
	handlerReturned := make(chan struct{})
	go func() {
		gateway.ServeHTTP(httptest.NewRecorder(), request)
		close(handlerReturned)
	}()
	<-backend.started
	cancelRequest()
	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		t.Fatal("HTTP handler did not stop waiting after cancellation")
	}

	drained := make(chan error, 1)
	go func() { drained <- gateway.CloseAndDrain(context.Background(), registration) }()
	select {
	case err := <-drained:
		t.Fatalf("CloseAndDrain() returned before durable completion: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(backend.release)
	select {
	case <-durable.unknownObserved:
	case <-time.After(time.Second):
		t.Fatal("canceled mutation was not marked UNKNOWN durably")
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("CloseAndDrain() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CloseAndDrain() did not return after durable UNKNOWN finalization")
	}
	if canceled := <-backend.contextCanceled; !canceled {
		t.Fatal("backend operation did not inherit drain cancellation")
	}
}

func TestCloseAndDrainRejectsPostCloseCallsAndIsIdempotent(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	backend := &recordingBackend{result: json.RawMessage(`{"number":12}`)}
	gateway := newTestGateway(t, now, &fakeStore{}, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	if err := gateway.CloseAndDrain(context.Background(), registration); err != nil {
		t.Fatalf("CloseAndDrain() error = %v", err)
	}
	if err := gateway.CloseAndDrain(context.Background(), registration); err != nil {
		t.Fatalf("second CloseAndDrain() error = %v", err)
	}

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":16,"method":"tools/call","params":{"name":"get_issue","arguments":{}}}`))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("post-close tools/call response = %d %s", response.Code, response.Body.String())
	}
	if backend.count() != 0 {
		t.Fatalf("post-close backend calls = %d, want zero", backend.count())
	}
}

func TestCloseAndDrainTimeoutDoesNotReauthorizeRegistration(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	backend := &blockingBackend{started: make(chan struct{}), release: make(chan struct{}), contextCanceled: make(chan bool, 1)}
	gateway := newTestGateway(t, now, &fakeStore{}, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	handlerReturned := make(chan struct{})
	go func() {
		body := `{"jsonrpc":"2.0","id":17,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"timed-drain","body":"Wait"}}}`
		gateway.ServeHTTP(httptest.NewRecorder(), rpcRequest(t, registration, mcp.ProtocolVersion, body))
		close(handlerReturned)
	}()
	<-backend.started

	drainContext, cancelDrain := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelDrain()
	if err := gateway.CloseAndDrain(drainContext, registration); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseAndDrain() error = %v, want context.DeadlineExceeded", err)
	}
	assertUnauthorizedCall(t, gateway, registration)

	close(backend.release)
	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		t.Fatal("admitted mutation did not finish")
	}
	if err := gateway.CloseAndDrain(context.Background(), registration); err != nil {
		t.Fatalf("CloseAndDrain() after completion error = %v", err)
	}
	assertUnauthorizedCall(t, gateway, registration)
}

func TestCloseAndDrainPreventsBackendStartAfterAdmissionClosesDuringReservation(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{
		reserveStarted: make(chan struct{}), releaseReserve: make(chan struct{}),
		unknownObserved: make(chan mutationContextObservation, 1),
	}
	backend := &recordingBackend{result: json.RawMessage(`{"comment_id":99}`)}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	handlerReturned := make(chan struct{})
	go func() {
		body := `{"jsonrpc":"2.0","id":18,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"closing-reservation","body":"Wait"}}}`
		gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, body))
		close(handlerReturned)
	}()
	<-durable.reserveStarted

	drained := make(chan error, 1)
	go func() { drained <- gateway.CloseAndDrain(context.Background(), registration) }()
	select {
	case err := <-drained:
		t.Fatalf("CloseAndDrain() returned while durable reservation was in progress: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(durable.releaseReserve)
	select {
	case <-durable.unknownObserved:
	case <-time.After(time.Second):
		t.Fatal("closed-admission reservation was not marked UNKNOWN")
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("CloseAndDrain() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("CloseAndDrain() did not return after durable UNKNOWN finalization")
	}
	select {
	case <-handlerReturned:
	case <-time.After(time.Second):
		t.Fatal("mutation handler did not return")
	}
	if !strings.Contains(response.Body.String(), `"isError":true`) {
		t.Fatalf("mutation response = %d %s", response.Code, response.Body.String())
	}
	if backend.count() != 0 {
		t.Fatalf("backend calls after admission closed = %d, want zero", backend.count())
	}
	reserved, started, completed, failed, unknown := durable.mutationCounts()
	if reserved != 1 || started != 1 || completed != 0 || failed != 0 || unknown != 1 {
		t.Fatalf("mutation lifecycle counts = reserve %d, start %d, complete %d, fail %d, unknown %d", reserved, started, completed, failed, unknown)
	}
}

func TestMutationsAreSerializedPerAgentSession(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{}
	backend := &serialBackend{entered: make(chan string, 2), releaseFirst: make(chan struct{})}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	call := func(operationID string, done chan<- struct{}) {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":%q,"body":"comment"}}}`, operationID)
		gateway.ServeHTTP(httptest.NewRecorder(), rpcRequest(t, registration, mcp.ProtocolVersion, body))
		done <- struct{}{}
	}
	done := make(chan struct{}, 2)
	go call("first", done)
	if operationID := <-backend.entered; operationID != testMutationID(1) {
		t.Fatalf("first backend operation = %q", operationID)
	}
	go call("second", done)
	select {
	case operationID := <-backend.entered:
		t.Fatalf("operation %q entered while first mutation was active", operationID)
	case <-time.After(100 * time.Millisecond):
	}
	_, started, _, _, _ := durable.mutationCounts()
	if started != 1 {
		t.Fatalf("started mutations before release = %d, want 1", started)
	}
	close(backend.releaseFirst)
	if operationID := <-backend.entered; operationID != testMutationID(2) {
		t.Fatalf("second backend operation = %q", operationID)
	}
	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("serialized mutation did not finish")
		}
	}
}

func TestEveryListedOperationRevalidatesTheTurnFence(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{}
	backend := &recordingBackend{}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)
	durable.setValidateError(store.ErrAgentTurnFenceLost)

	requests := []string{
		`{"jsonrpc":"2.0","id":8,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"get_issue","arguments":{}}}`,
	}
	for _, body := range requests {
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, body))
		if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "lease-secret") {
			t.Fatalf("stale fence response = %d %s", response.Code, response.Body.String())
		}
	}
	if durable.validationCount() != 4 || backend.count() != 0 {
		t.Fatalf("stale fence calls = %d, backend calls = %d", durable.validationCount(), backend.count())
	}
}

func TestTransportRejectsOriginAndInvalidContentNegotiation(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	gateway := newTestGateway(t, now, &fakeStore{}, fakeBackend{})
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{name: "Origin", mutate: func(request *http.Request) { request.Header.Set("Origin", "https://attacker.example") }, want: http.StatusForbidden},
		{name: "JSON not accepted", mutate: func(request *http.Request) { request.Header.Set("Accept", "application/json;q=0, text/event-stream") }, want: http.StatusNotAcceptable},
		{name: "event stream not accepted", mutate: func(request *http.Request) { request.Header.Set("Accept", "application/json, text/event-stream;q=0.0") }, want: http.StatusNotAcceptable},
		{name: "wrong content type", mutate: func(request *http.Request) { request.Header.Set("Content-Type", "text/plain") }, want: http.StatusUnsupportedMediaType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := rpcRequest(t, registration, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
			test.mutate(request)
			response := httptest.NewRecorder()
			gateway.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.want, response.Body.String())
			}
		})
	}
}

func TestTransportRejectsOversizedRequests(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: &fakeStore{}, Backend: fakeBackend{},
		MaxRequestBytes: 128, Now: func() time.Time { return now }, Random: bytes.NewReader(bytes.Repeat([]byte{0x44}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	oversized := rpcRequest(t, registration, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"padding":"`+strings.Repeat("x", 256)+`"}}`)
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, oversized)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized request status = %d, body = %s", response.Code, response.Body.String())
	}

}

func TestNewRejectsInvalidMutationFenceCheckInterval(t *testing.T) {
	const maximum = 365 * 24 * time.Hour
	for _, interval := range []time.Duration{-time.Second, time.Nanosecond, maximum + time.Microsecond} {
		_, err := mcp.New(mcp.Config{
			EndpointURL: "https://gateway.internal/mcp", Store: &fakeStore{}, Backend: fakeBackend{},
			MutationFenceCheckInterval: interval,
		})
		if !errors.Is(err, mcp.ErrInvalidConfiguration) {
			t.Errorf("New() interval %s error = %v, want ErrInvalidConfiguration", interval, err)
		}
	}

	for _, interval := range []time.Duration{0, time.Microsecond, maximum} {
		if _, err := mcp.New(mcp.Config{
			EndpointURL: "https://gateway.internal/mcp", Store: &fakeStore{}, Backend: fakeBackend{},
			MutationFenceCheckInterval: interval,
		}); err != nil {
			t.Errorf("New() interval %s error = %v", interval, err)
		}
	}
}

func TestNewRejectsInvalidMutationFinalizationTimeout(t *testing.T) {
	const maximum = 365 * 24 * time.Hour
	for _, timeout := range []time.Duration{-time.Second, time.Nanosecond, maximum + time.Microsecond} {
		_, err := mcp.New(mcp.Config{
			EndpointURL: "https://gateway.internal/mcp", Store: &fakeStore{}, Backend: fakeBackend{},
			MutationFinalizationTimeout: timeout,
		})
		if !errors.Is(err, mcp.ErrInvalidConfiguration) {
			t.Errorf("New() timeout %s error = %v, want ErrInvalidConfiguration", timeout, err)
		}
	}

	for _, timeout := range []time.Duration{0, time.Microsecond, maximum} {
		if _, err := mcp.New(mcp.Config{
			EndpointURL: "https://gateway.internal/mcp", Store: &fakeStore{}, Backend: fakeBackend{},
			MutationFinalizationTimeout: timeout,
		}); err != nil {
			t.Errorf("New() timeout %s error = %v", timeout, err)
		}
	}
}

func TestNewRejectsInvalidMutationOperationTimeout(t *testing.T) {
	const maximum = 365 * 24 * time.Hour
	for _, timeout := range []time.Duration{-time.Second, time.Nanosecond, maximum + time.Microsecond} {
		_, err := mcp.New(mcp.Config{
			EndpointURL: "https://gateway.internal/mcp", Store: &fakeStore{}, Backend: fakeBackend{},
			MutationOperationTimeout: timeout,
		})
		if !errors.Is(err, mcp.ErrInvalidConfiguration) {
			t.Errorf("New() timeout %s error = %v, want ErrInvalidConfiguration", timeout, err)
		}
	}

	for _, timeout := range []time.Duration{0, time.Microsecond, maximum} {
		if _, err := mcp.New(mcp.Config{
			EndpointURL: "https://gateway.internal/mcp", Store: &fakeStore{}, Backend: fakeBackend{},
			MutationOperationTimeout: timeout,
		}); err != nil {
			t.Errorf("New() timeout %s error = %v", timeout, err)
		}
	}
}

func TestRegisteredTokenFollowsLiveStoreFencePastCapturedExpiry(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	current := now
	durable := &fakeStore{}
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp", Store: durable, Backend: fakeBackend{},
		Now: func() time.Time { return current }, Random: bytes.NewReader(bytes.Repeat([]byte{0x45}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	current = now.Add(2 * time.Hour)
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"opencode","version":"1.0.0"}}}`))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"protocolVersion":"2025-11-25"`) {
		t.Fatalf("heartbeat-extended token response = %d, body = %s", response.Code, response.Body.String())
	}
	if durable.validationCount() != 1 {
		t.Fatalf("ValidateTurnFence calls = %d, want one", durable.validationCount())
	}
}

func TestInitializeRejectsTokenWhenStoreFenceIsStale(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{validateErr: errors.New("store-secret stale fence")}
	gateway := newTestGateway(t, now, durable, fakeBackend{})
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"opencode","version":"1.0.0"}}}`))
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), "store-secret") {
		t.Fatalf("stale initialize response = %d, body = %s", response.Code, response.Body.String())
	}
	if durable.validationCount() != 1 {
		t.Fatalf("ValidateTurnFence calls = %d, want one", durable.validationCount())
	}
}

func TestKnownBackendMutationFailureIsDurablyFailed(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{}
	backend := &recordingBackend{err: errors.New("credential-bearing backend detail")}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"known-failure","body":"comment"}}}`))
	if !strings.Contains(response.Body.String(), `"isError":true`) || strings.Contains(response.Body.String(), "credential-bearing") {
		t.Fatalf("known failure response = %s", response.Body.String())
	}
	_, _, completed, failed, unknown := durable.mutationCounts()
	if completed != 0 || failed != 1 || unknown != 0 {
		t.Fatalf("known failure states = complete %d, fail %d, unknown %d", completed, failed, unknown)
	}
	if err := gateway.CloseAndDrain(context.Background(), registration); err != nil {
		t.Fatalf("CloseAndDrain() error = %v for durable failed mutation", err)
	}
}

func TestStartFailureIsUnresolvedWhenFailureTransitionCannotBeRecorded(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{
		startErr: errors.New("start database-secret"),
		failErr:  errors.New("fail database-secret"),
	}
	backend := &recordingBackend{result: json.RawMessage(`{"comment_id":42}`)}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"start-persistence-failure","body":"comment"}}}`))
	if !strings.Contains(response.Body.String(), `mutation durable state is unresolved`) || strings.Contains(response.Body.String(), "database-secret") {
		t.Fatalf("transition failure response = %s", response.Body.String())
	}
	if backend.count() != 0 {
		t.Fatalf("backend calls = %d, want zero", backend.count())
	}
	_, _, _, failed, _ := durable.mutationCounts()
	if failed != 1 {
		t.Fatalf("FailMutation calls = %d, want one", failed)
	}
	for call := 1; call <= 2; call++ {
		err := gateway.CloseAndDrain(context.Background(), registration)
		if !errors.Is(err, mcp.ErrMutationDrainUnresolved) || strings.Contains(err.Error(), "database-secret") {
			t.Fatalf("CloseAndDrain() call %d error = %v, want sanitized ErrMutationDrainUnresolved", call, err)
		}
	}
}

func TestAmbiguousMutationReportsUnresolvedDurableStateWhenMarkUnknownFails(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{unknownErr: errors.New("unknown database-secret")}
	backend := &recordingBackend{err: mcp.OutcomeUnknown(errors.New("backend-secret timeout"))}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"mark-unknown-failure","body":"comment"}}}`))
	if !strings.Contains(response.Body.String(), `mutation durable state is unresolved`) || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("MarkMutationUnknown failure response = %s", response.Body.String())
	}
	_, _, _, _, unknown := durable.mutationCounts()
	if unknown != 1 {
		t.Fatalf("MarkMutationUnknown calls = %d, want one", unknown)
	}
	assertUnresolvedDrain(t, gateway, registration)
}

func TestKnownMutationFailureReportsUnresolvedDurableStateWhenFailTransitionFails(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{failErr: errors.New("fail database-secret")}
	backend := &recordingBackend{err: errors.New("backend-secret rejection")}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"fail-persistence-failure","body":"comment"}}}`))
	if !strings.Contains(response.Body.String(), `mutation durable state is unresolved`) || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("FailMutation failure response = %s", response.Body.String())
	}
	_, _, _, failed, _ := durable.mutationCounts()
	if failed != 1 {
		t.Fatalf("FailMutation calls = %d, want one", failed)
	}
	assertUnresolvedDrain(t, gateway, registration)
}

func TestCompletionFailureReportsUnresolvedDurableStateWhenMarkUnknownAlsoFails(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{
		completeErr: errors.New("complete database-secret"),
		unknownErr:  errors.New("unknown database-secret"),
	}
	backend := &recordingBackend{result: json.RawMessage(`{"comment_id":42}`)}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":14,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"complete-mark-unknown-failure","body":"comment"}}}`))
	if !strings.Contains(response.Body.String(), `mutation durable state is unresolved`) || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("completion transition failure response = %s", response.Body.String())
	}
	_, _, completed, _, unknown := durable.mutationCounts()
	if completed != 1 || unknown != 1 {
		t.Fatalf("transition calls = complete %d, unknown %d; want one each", completed, unknown)
	}
	assertUnresolvedDrain(t, gateway, registration)
}

func TestCloseAndDrainAggregatesUnresolvedStatusAcrossAdmittedMutations(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	durable := &fakeStore{failErr: errors.New("first transition database-secret")}
	backend := &recordingBackend{err: errors.New("known backend failure")}
	gateway := newTestGateway(t, now, durable, backend)
	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	initialize(t, gateway, registration)

	for index := 1; index <= 2; index++ {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"comment_on_issue","arguments":{"operation_id":"aggregate-%d","body":"comment"}}}`, index, index)
		response := httptest.NewRecorder()
		gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, body))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"isError":true`) {
			t.Fatalf("mutation %d response = %d %s", index, response.Code, response.Body.String())
		}
		if index == 1 {
			durable.setFailError(nil)
		}
	}

	assertUnresolvedDrain(t, gateway, registration)
}

func TestRegistrationProvidesAuthenticatedOpenCodeCompatibleInitialization(t *testing.T) {
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp",
		Store:       &fakeStore{},
		Backend:     fakeBackend{},
		Now:         func() time.Time { return now },
		Random:      bytes.NewReader(bytes.Repeat([]byte{0x5a}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	registration, err := gateway.Register(validScope(now))
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if registration.Server.Type != "http" || registration.Server.Name != "omnigrex" || registration.Server.URL != "https://gateway.internal/mcp" {
		t.Fatalf("MCP server descriptor = %#v", registration.Server)
	}
	if len(registration.Server.Headers) != 1 || registration.Server.Headers[0].Name != "Authorization" || registration.Server.Headers[0].Value == "" {
		t.Fatalf("MCP Authorization header = %#v", registration.Server.Headers)
	}

	request := rpcRequest(t, registration, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"opencode","version":"1.0.0"}}}`)
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Result  struct {
			ProtocolVersion string `json:"protocolVersion"`
			Capabilities    struct {
				Tools struct {
					ListChanged bool `json:"listChanged"`
				} `json:"tools"`
			} `json:"capabilities"`
			ServerInfo struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	if payload.JSONRPC != "2.0" || payload.ID != 1 || payload.Result.ProtocolVersion != "2025-11-25" || payload.Result.ServerInfo.Name != "omnigrex" || payload.Result.Capabilities.Tools.ListChanged {
		t.Fatalf("initialize response = %#v", payload)
	}
}

func validScope(now time.Time) mcp.TokenScope {
	return mcp.TokenScope{
		Lease: store.AgentTurnLease{
			AgentTurn: store.AgentTurn{
				AgentTurnSpec: store.AgentTurnSpec{AgentSessionID: "session-1", AgentProfileConfig: json.RawMessage(`{"role":"DEVELOPER"}`)},
				ID:            "turn-1", AgentAssignmentID: "assignment-1", ExecutionEpoch: 4, CreatedAt: now,
			},
			JobLease: store.JobLease{Job: store.Job{JobSpec: store.JobSpec{WorkflowID: "workflow-1", AgentSessionID: "session-1", AgentTurnID: "turn-1", AgentAssignmentID: "assignment-1", ExecutionEpoch: 4}}},
			OwnerID:  "worker-1", OwnerToken: "lease-secret", LeaseExpiresAt: now.Add(time.Hour),
		},
		WorkflowID:    "workflow-1",
		Role:          workflow.RoleDeveloper,
		Repository:    mcp.RepositoryScope{ID: 9123, Owner: "acme", Name: "widgets"},
		Issue:         mcp.IssueScope{ID: 456, Number: 12},
		Branch:        "omnigrex/issue-12",
		DefaultBranch: "main",
		HeadSHA:       "0123456789abcdef",
		ExpiresAt:     now.Add(30 * time.Minute),
	}
}

func rpcRequest(t *testing.T, registration mcp.Registration, protocolVersion, body string) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewBufferString(body))
	request.Header.Set("Authorization", registration.Server.Headers[0].Value)
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	if protocolVersion != "" {
		request.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	return request
}

func assertUnauthorizedCall(t *testing.T, gateway *mcp.Gateway, registration mcp.Registration) {
	t.Helper()
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","id":19,"method":"tools/call","params":{"name":"get_issue","arguments":{}}}`))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("closed registration response = %d %s", response.Code, response.Body.String())
	}
}

func assertUnresolvedDrain(t *testing.T, gateway *mcp.Gateway, registration mcp.Registration) {
	t.Helper()
	err := gateway.CloseAndDrain(context.Background(), registration)
	if !errors.Is(err, mcp.ErrMutationDrainUnresolved) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("CloseAndDrain() error = %v, want sanitized ErrMutationDrainUnresolved", err)
	}
}

func newTestGateway(t *testing.T, now time.Time, durable *fakeStore, backend mcp.Backend) *mcp.Gateway {
	t.Helper()
	gateway, err := mcp.New(mcp.Config{
		EndpointURL: "https://gateway.internal/mcp",
		Store:       durable, Backend: backend,
		Now:    func() time.Time { return now },
		Random: bytes.NewReader(bytes.Repeat([]byte{byte(1 + now.Second())}, 32)),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return gateway
}

func initialize(t *testing.T, gateway *mcp.Gateway, registration mcp.Registration) {
	t.Helper()
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"opencode","version":"1.0.0"}}}`))
	if response.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, body = %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	gateway.ServeHTTP(response, rpcRequest(t, registration, mcp.ProtocolVersion, `{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`))
	if response.Code != http.StatusAccepted {
		t.Fatalf("notifications/initialized status = %d, body = %s", response.Code, response.Body.String())
	}
}

type fakeStore struct {
	mutex              sync.Mutex
	validations        int
	validationLeases   []store.AgentTurnLease
	validateErr        error
	mutations          map[string]store.MutationReservation
	reserved           int
	started            int
	completed          int
	failed             int
	unknown            int
	specs              []store.MutationSpec
	completionObserved chan struct{}
	startErr           error
	reserveErr         error
	completeErr        error
	failErr            error
	unknownErr         error
	unknownObserved    chan mutationContextObservation
	reserveStarted     chan struct{}
	releaseReserve     chan struct{}
}

type lifecycleContextKey struct{}

type mutationContextObservation struct {
	contextErr  error
	value       any
	hasDeadline bool
}

func (fake *fakeStore) ValidateTurnFence(_ context.Context, lease store.AgentTurnLease) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.validations++
	fake.validationLeases = append(fake.validationLeases, lease)
	return fake.validateErr
}

func (fake *fakeStore) validationCount() int {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.validations
}

func (fake *fakeStore) validationLeasesSnapshot() []store.AgentTurnLease {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return append([]store.AgentTurnLease(nil), fake.validationLeases...)
}

func (fake *fakeStore) setValidateError(err error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.validateErr = err
}

func (fake *fakeStore) setFailError(err error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.failErr = err
}

func (fake *fakeStore) ReserveMutation(_ context.Context, lease store.AgentTurnLease, spec store.MutationSpec) (store.MutationReservation, error) {
	if fake.reserveStarted != nil {
		close(fake.reserveStarted)
		<-fake.releaseReserve
	}
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.reserved++
	fake.specs = append(fake.specs, spec)
	if fake.mutations == nil {
		fake.mutations = make(map[string]store.MutationReservation)
	}
	if existing, ok := fake.mutations[spec.OperationID]; ok {
		return existing, nil
	}
	reservation := store.MutationReservation{
		ID: testMutationID(len(fake.mutations) + 1), AgentTurnID: lease.ID, ExecutionEpoch: lease.ExecutionEpoch,
		OperationID: spec.OperationID, ToolName: spec.ToolName, Request: spec.Request, State: store.MutationReserved,
	}
	fake.mutations[spec.OperationID] = reservation
	return reservation, fake.reserveErr
}

func (*fakeStore) AcknowledgeMutationReplay(context.Context, store.AgentTurnLease, string, store.MutationSpec) error {
	return nil
}

func (fake *fakeStore) mutationSpecs() []store.MutationSpec {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return append([]store.MutationSpec(nil), fake.specs...)
}

func (fake *fakeStore) StartMutation(_ context.Context, _ store.AgentTurnLease, mutationID string) (store.MutationReservation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.startErr != nil {
		return store.MutationReservation{}, fake.startErr
	}
	for operationID, mutation := range fake.mutations {
		if mutation.ID == mutationID {
			fake.started++
			mutation.State = store.MutationInFlight
			fake.mutations[operationID] = mutation
			return mutation, nil
		}
	}
	return store.MutationReservation{}, errors.New("mutation not found")
}

func (fake *fakeStore) CompleteMutation(_ context.Context, _ store.AgentTurnLease, mutationID string, result json.RawMessage) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if fake.completeErr != nil {
		fake.completed++
		return fake.completeErr
	}
	for operationID, mutation := range fake.mutations {
		if mutation.ID == mutationID {
			fake.completed++
			mutation.State = store.MutationSucceeded
			mutation.Result = append(json.RawMessage(nil), result...)
			fake.mutations[operationID] = mutation
			if fake.completionObserved != nil {
				fake.completionObserved <- struct{}{}
			}
			return nil
		}
	}
	return errors.New("mutation not found")
}

func (fake *fakeStore) FailMutation(_ context.Context, _ store.AgentTurnLease, mutationID string, _ error) error {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.failed++
	if fake.failErr != nil {
		return fake.failErr
	}
	return fake.setMutationState(mutationID, store.MutationFailed)
}

func (fake *fakeStore) MarkMutationUnknown(ctx context.Context, _ store.AgentTurnLease, mutationID string, _ error) error {
	if fake.unknownObserved != nil {
		_, hasDeadline := ctx.Deadline()
		fake.unknownObserved <- mutationContextObservation{
			contextErr: ctx.Err(), value: ctx.Value(lifecycleContextKey{}), hasDeadline: hasDeadline,
		}
	}
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.unknown++
	if fake.unknownErr != nil {
		return fake.unknownErr
	}
	return fake.setMutationState(mutationID, store.MutationUnknown)
}

func (fake *fakeStore) mutationState(operationID string) store.MutationState {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.mutations[operationID].State
}

func (fake *fakeStore) setMutationState(mutationID string, state store.MutationState) error {
	for operationID, mutation := range fake.mutations {
		if mutation.ID == mutationID {
			mutation.State = state
			fake.mutations[operationID] = mutation
			return nil
		}
	}
	return errors.New("mutation not found")
}

func (fake *fakeStore) mutationCounts() (int, int, int, int, int) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.reserved, fake.started, fake.completed, fake.failed, fake.unknown
}

type fakeBackend struct{}

func (fakeBackend) Execute(context.Context, mcp.Invocation) (json.RawMessage, error) {
	return nil, errors.New("unexpected tool call")
}

type recordingBackend struct {
	mutex  sync.Mutex
	calls  []mcp.Invocation
	result json.RawMessage
	err    error
}

func (backend *recordingBackend) Execute(_ context.Context, invocation mcp.Invocation) (json.RawMessage, error) {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	backend.calls = append(backend.calls, invocation)
	return backend.result, backend.err
}

func (backend *recordingBackend) count() int {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	return len(backend.calls)
}

func (backend *recordingBackend) singleCall(t *testing.T) mcp.Invocation {
	t.Helper()
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	if len(backend.calls) != 1 {
		t.Fatalf("backend calls = %#v, want one", backend.calls)
	}
	return backend.calls[0]
}

type recordingLedger struct {
	mutex   sync.Mutex
	records []mcp.ReadRecord
	err     error
}

type blockingBackend struct {
	started         chan struct{}
	release         chan struct{}
	contextCanceled chan bool
}

type cancelingBackend struct {
	started  chan struct{}
	finished chan struct{}
}

func (backend *cancelingBackend) Execute(ctx context.Context, _ mcp.Invocation) (json.RawMessage, error) {
	close(backend.started)
	<-ctx.Done()
	close(backend.finished)
	return nil, ctx.Err()
}

func (backend *blockingBackend) Execute(ctx context.Context, _ mcp.Invocation) (json.RawMessage, error) {
	close(backend.started)
	<-backend.release
	backend.contextCanceled <- ctx.Err() != nil
	return json.RawMessage(`{"comment_id":99}`), nil
}

type serialBackend struct {
	entered      chan string
	releaseFirst chan struct{}
}

type releasingBackend struct {
	mutex          sync.Mutex
	started        chan struct{}
	executeRelease chan struct{}
	released       chan mcp.ToolScope
	releases       int
}

func (backend *releasingBackend) Execute(_ context.Context, _ mcp.Invocation) (json.RawMessage, error) {
	if backend.started != nil {
		close(backend.started)
	}
	if backend.executeRelease != nil {
		<-backend.executeRelease
	}
	return json.RawMessage(`{"comment_id":99}`), nil
}

func (backend *releasingBackend) ReleaseTurn(scope mcp.ToolScope) {
	backend.mutex.Lock()
	backend.releases++
	backend.mutex.Unlock()
	backend.released <- scope
}

func (backend *releasingBackend) releaseCount() int {
	backend.mutex.Lock()
	defer backend.mutex.Unlock()
	return backend.releases
}

func (backend *serialBackend) Execute(_ context.Context, invocation mcp.Invocation) (json.RawMessage, error) {
	backend.entered <- invocation.OperationID
	if invocation.OperationID == testMutationID(1) {
		<-backend.releaseFirst
	}
	return json.RawMessage(fmt.Sprintf(`{"operation_id":%q}`, invocation.OperationID)), nil
}

func testMutationID(index int) string {
	return fmt.Sprintf("10000000-0000-4000-8000-%012d", index)
}

func (ledger *recordingLedger) RecordRead(_ context.Context, _ store.AgentTurnLease, record mcp.ReadRecord) error {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	ledger.records = append(ledger.records, record)
	return ledger.err
}

func (ledger *recordingLedger) count() int {
	ledger.mutex.Lock()
	defer ledger.mutex.Unlock()
	return len(ledger.records)
}

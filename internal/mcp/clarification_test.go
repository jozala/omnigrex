package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jozala/omnigrex/internal/role"
	"strings"
	"testing"

	"github.com/jozala/omnigrex/internal/collaboration"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func (f *clarificationLedger) HasPendingHumanInput(context.Context, string, string) (bool, error) {
	return false, nil
}

type clarificationLedger struct {
	resolutionOperation string
	round               collaboration.Round
	published           bool
	source              collaboration.Source
}

func (f *clarificationLedger) RequestClarification(_ context.Context, _ store.AgentTurnLease, id string, questions []collaboration.Question) (collaboration.Round, error) {
	f.round = collaboration.Round{Reference: "R1", ParticipantID: reconciliationContext().Turn.AgentParticipantID, TurnID: reconciliationContext().Turn.ID, ID: "R" + id, Questions: questions}
	return f.round, nil
}
func (f *clarificationLedger) ConfirmClarificationPublication(_ context.Context, _, id string, source collaboration.Source) error {
	if id != f.round.ID || source.ID == "" {
		panic("incorrect publication correlation")
	}
	f.published = true
	f.source = source
	return nil
}
func (f *clarificationLedger) GetClarificationRound(context.Context, string, string) (collaboration.Round, error) {
	return f.round, nil
}
func (f *clarificationLedger) ResolveClarification(_ context.Context, _ store.AgentTurnLease, operation, _ string, _ []collaboration.Resolution) error {
	f.resolutionOperation = operation
	return nil
}
func TestSignedClarificationPublishesBeforeReturningWaiting(t *testing.T) {
	for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		t.Run(string(role), func(t *testing.T) {
			ledger := &clarificationLedger{}
			api := &backendGitHub{}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: &backendCredentials{developer: "developer-secret"}, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger})
			if err != nil {
				t.Fatal(err)
			}
			result, err := backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolRequestClarification, Class: mcp.MutationTool, Scope: productionToolScope(role), OperationID: reconciliationMutationID, Arguments: json.RawMessage(`{"operation_id":"ask1","questions":[{"id":"Q1","body":"Which compatibility policy?"}],"signature":"persisted-signature"}`)})
			if err != nil {
				t.Fatal(err)
			}
			if !ledger.published || !strings.Contains(string(result), "CLARIFICATION_WAIT") || !strings.Contains(api.issueCommentRequest.Body, "persisted-signature") || !strings.Contains(api.issueCommentRequest.Body, ledger.round.Reference) {
				t.Fatalf("publication=%+v result=%s", api.issueCommentRequest, result)
			}
		})
	}
}

type clarificationLostGitHub struct {
	*backendGitHub
	posted githubapi.IssueComment
	posts  int
	tamper bool
}

func (f *clarificationLostGitHub) CreateIssueComment(_ context.Context, _ string, _, _ string, _ int, request githubapi.CommentRequest) (githubapi.IssueComment, error) {
	f.posts++
	f.posted = githubapi.IssueComment{ID: 701, NodeID: "IC_701", HTMLURL: "https://github.test/acme/widgets/issues/12#issuecomment-701", Body: githubapi.JoinBodyParts(request.Body, request.Marker)}
	return githubapi.IssueComment{}, context.DeadlineExceeded
}
func (f *clarificationLostGitHub) ListIssueComments(context.Context, string, string, string, int) ([]githubapi.IssueComment, error) {
	comment := f.posted
	if f.tamper {
		comment.Body += " changed"
	}
	return []githubapi.IssueComment{comment}, nil
}
func TestClarificationLostPublicationResponseReconcilesWithoutAnotherPost(t *testing.T) {
	ledger := &clarificationLedger{}
	api := &clarificationLostGitHub{backendGitHub: &backendGitHub{}}
	credentials := &backendCredentials{developer: "developer-secret"}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: credentials, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger})
	if err != nil {
		t.Fatal(err)
	}
	request := json.RawMessage(`{"operation_id":"ask1","questions":[{"id":"Q1","body":"Which policy?"}],"signature":"persisted-signature"}`)
	_, err = backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolRequestClarification, Class: mcp.MutationTool, Scope: productionToolScope(workflow.RoleDeveloper), OperationID: reconciliationMutationID, Arguments: request})
	if !mcp.IsOutcomeUnknown(err) || ledger.published {
		t.Fatalf("uncertain publication acted delivered: %t %v", ledger.published, err)
	}
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: api, Credentials: credentials, Publications: &reconciliationPublications{}, Clarifications: ledger})
	if err != nil {
		t.Fatal(err)
	}
	mutation := reconciliationMutation(mcp.ToolRequestClarification, string(request))
	mutation.ExternalResourceID = "9123:456"
	api.tamper = true
	result, err := reconciler.Reconcile(context.Background(), reconciliationContext(), mutation)
	if err != nil || result.Disposition != mcp.ReconciliationUnresolved || ledger.published {
		t.Fatalf("changed question was accepted: %+v %v", result, err)
	}
	api.tamper = false
	result, err = reconciler.Reconcile(context.Background(), reconciliationContext(), mutation)
	if err != nil || result.Disposition != mcp.ReconciliationFound || !ledger.published || api.posts != 1 || !strings.Contains(string(result.Outcome.Result), ledger.round.ID) {
		t.Fatalf("recovery=%+v %v posts=%d", result, err, api.posts)
	}
}

type alternateClarificationPublisher struct {
	source collaboration.Source
	posts  int
}

func (p *alternateClarificationPublisher) Validate(collaboration.PublicationMessage) error {
	return nil
}
func (p *alternateClarificationPublisher) Publish(context.Context, collaboration.PublicationMessage) (collaboration.Source, error) {
	p.posts++
	p.source = collaboration.Source{Channel: "web", ID: "question-view"}
	return p.source, nil
}
func (p *alternateClarificationPublisher) Reconcile(context.Context, collaboration.PublicationMessage) (collaboration.Source, bool, error) {
	return p.source, true, nil
}
func TestClarificationMCPUsesAlternatePublisherForExecutionAndRecovery(t *testing.T) {
	ledger := &clarificationLedger{}
	publisher := &alternateClarificationPublisher{}
	// These are required by other backend tools, but must not be used by clarification.
	api := &backendGitHub{}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: &backendCredentials{}, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger, ClarificationPublisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	request := json.RawMessage(`{"operation_id":"ask1","questions":[{"id":"Q1","body":"Policy?"}]}`)
	_, err = backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolRequestClarification, Class: mcp.MutationTool, Scope: productionToolScope(workflow.RoleDeveloper), OperationID: reconciliationMutationID, Arguments: request})
	if err != nil || !ledger.published || ledger.source.Channel != "web" || api.issueCommentRequest.Body != "" {
		t.Fatal("alternate delivery touched GitHub", err)
	}
	ledger.published = false
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: api, Credentials: &backendCredentials{}, Publications: &reconciliationPublications{}, Clarifications: ledger, ClarificationPublisher: publisher})
	if err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.Reconcile(context.Background(), reconciliationContext(), reconciliationMutation(mcp.ToolRequestClarification, string(request)))
	if err != nil || result.Disposition != mcp.ReconciliationFound || !ledger.published || publisher.posts != 1 {
		t.Fatal(result, err)
	}
}

func (f *clarificationLedger) GetCollaborationRecord(_ context.Context, _, _, _, collection string, index, offset int) (collaboration.RecordPage, error) {
	if collection != "human_input" || index != 1 || offset != 4096 {
		return collaboration.RecordPage{}, collaboration.ErrInvalid
	}
	return collaboration.RecordPage{Content: "second record, second chunk", Index: index, Total: 2}, nil
}
func TestCollaborationReadAdmitsNonzeroRecordAndChunkOffsets(t *testing.T) {
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: &backendGitHub{}, Credentials: &backendCredentials{}, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: &clarificationLedger{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolGetCollaborationRecord, Class: mcp.ReadTool, Scope: productionToolScope(workflow.RoleDeveloper), Arguments: json.RawMessage(`{"collection":"human_input","index":1,"offset":4096}`)})
	if err != nil || !strings.Contains(string(result), "second record, second chunk") {
		t.Fatalf("pagination rejected: %s %v", result, err)
	}
}

func TestResolutionReconciliationMatchesIntentWithOrchestratorCoverage(t *testing.T) {
	resolution := collaboration.Resolution{QuestionID: "Q1", ResponseIDs: []string{"A3"}, Explanation: "The explicit correction governs"}
	actual := resolution
	actual.ConsideredResponseIDs = []string{"A2", "A3"}
	actual.OriginOperationID = reconciliationMutationID
	ledger := &clarificationLedger{round: collaboration.Round{ID: "R1", ParticipantID: "assignment-1", Resolutions: []collaboration.Resolution{actual}}}
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &backendGitHub{}, Credentials: &backendCredentials{}, Publications: &reconciliationPublications{}, Clarifications: ledger})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]any{"operation_id": "resolve", "round_id": "R1", "resolutions": []collaboration.Resolution{resolution}})
	scope := reconciliationContext()
	mutation := reconciliationMutation("resolve_clarification", string(request))
	mutation.ExternalService = "omnigrex"
	mutation.ExternalResourceID = scope.WorkflowID
	result, err := reconciler.Reconcile(context.Background(), scope, mutation)
	if err != nil || result.Disposition != mcp.ReconciliationFound {
		t.Fatalf("derived coverage prevented exact intent recovery: %+v %v", result, err)
	}
	ledger.round.Resolutions[0].Explanation = "A different interpretation"
	result, err = reconciler.Reconcile(context.Background(), scope, mutation)
	if err != nil || result.Disposition != mcp.ReconciliationUnresolved {
		t.Fatalf("different interpretation reconciled: %+v %v", result, err)
	}
}

func TestResolutionRecoveryDoesNotBorrowIdenticalHistoricalEffect(t *testing.T) {
	resolution := collaboration.Resolution{QuestionID: "Q1", ResponseIDs: []string{"A2"}, Explanation: "The explicit correction governs"}
	ledger := &clarificationLedger{round: collaboration.Round{ID: "R1", ParticipantID: "assignment-1", Resolutions: []collaboration.Resolution{resolution}}}
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &backendGitHub{}, Credentials: &backendCredentials{}, Publications: &reconciliationPublications{}, Clarifications: ledger})
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(map[string]any{"operation_id": "cancelled-new-operation", "round_id": "R1", "resolutions": []collaboration.Resolution{resolution}})
	scope := reconciliationContext()
	mutation := reconciliationMutation("resolve_clarification", string(request))
	mutation.ExternalService = "omnigrex"
	mutation.ExternalResourceID = scope.WorkflowID
	for _, origin := range []string{"", "00000000-0000-4000-8000-000000000111"} {
		ledger.round.Resolutions[0].OriginOperationID = origin
		result, err := reconciler.Reconcile(context.Background(), scope, mutation)
		if err != nil || result.Disposition != mcp.ReconciliationUnresolved {
			t.Fatalf("new operation with no effect borrowed historical origin %q: %+v %v", origin, result, err)
		}
	}
}

func TestResolveClarificationForwardsOrchestratorReservationIdentity(t *testing.T) {
	ledger := &clarificationLedger{}
	backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: &backendGitHub{}, Credentials: &backendCredentials{}, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger})
	if err != nil {
		t.Fatal(err)
	}
	invocation := mcp.Invocation{Name: mcp.ToolResolveClarification, Class: mcp.MutationTool, Scope: productionToolScope(workflow.RoleDeveloper), OperationID: reconciliationMutationID, Arguments: json.RawMessage(`{"operation_id":"agent-label","round_id":"R1","resolutions":[{"question_id":"Q1","response_ids":["A1"],"explanation":"Explicit interpretation"}]}`)}
	if _, err := backend.Execute(context.Background(), invocation); err != nil {
		t.Fatal(err)
	}
	if ledger.resolutionOperation != reconciliationMutationID {
		t.Fatal("caller label was used instead of durable reservation identity")
	}
	ledger.resolutionOperation = ""
	invocation.Arguments = json.RawMessage(`{"operation_id":"empty","round_id":"R1","resolutions":[]}`)
	if _, err := backend.Execute(context.Background(), invocation); err == nil || ledger.resolutionOperation != "" {
		t.Fatal("empty interpretation batch was admitted")
	}
}

func TestResolutionRecoveryRequiresCompleteScopedBatchProof(t *testing.T) {
	wanted := []collaboration.Resolution{{QuestionID: "Q1", ResponseIDs: []string{"A1"}, Explanation: "First"}, {QuestionID: "Q2", ResponseIDs: []string{"A2"}, Explanation: "Second"}}
	for _, scenario := range []string{"complete", "partial", "other participant", "other round", "different intent"} {
		t.Run(scenario, func(t *testing.T) {
			actual := append([]collaboration.Resolution(nil), wanted...)
			for i := range actual {
				actual[i].OriginOperationID = reconciliationMutationID
			}
			round := collaboration.Round{ID: "R1", ParticipantID: "assignment-1", Resolutions: actual}
			switch scenario {
			case "partial":
				round.Resolutions[1].OriginOperationID = "other"
			case "other participant":
				round.ParticipantID = "other"
			case "other round":
				round.ID = "R2"
			case "different intent":
				round.Resolutions[1].Explanation = "Different"
			}
			ledger := &clarificationLedger{round: round}
			reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &backendGitHub{}, Credentials: &backendCredentials{}, Publications: &reconciliationPublications{}, Clarifications: ledger})
			if err != nil {
				t.Fatal(err)
			}
			request, _ := json.Marshal(map[string]any{"operation_id": "resolve", "round_id": "R1", "resolutions": wanted})
			scope := reconciliationContext()
			mutation := reconciliationMutation("resolve_clarification", string(request))
			mutation.ExternalService = "omnigrex"
			mutation.ExternalResourceID = scope.WorkflowID
			result, err := reconciler.Reconcile(context.Background(), scope, mutation)
			expected := mcp.ReconciliationUnresolved
			if scenario == "complete" {
				expected = mcp.ReconciliationFound
			}
			if err != nil || result.Disposition != expected {
				t.Fatalf("batch proof = %+v %v; want %s", result, err, expected)
			}
		})
	}
}

func TestPlannerQuestionRoundsAreBoundedBeforePublication(t *testing.T) {
	policies, err := role.NewBuiltinPolicyCatalog([]role.ID{role.Planner})
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range []int{1, 5, 6} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			ledger := &clarificationLedger{}
			api := &backendGitHub{}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: &backendCredentials{developer: "developer-secret"}, Publisher: &backendPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: ledger, Policies: policies})
			if err != nil {
				t.Fatal(err)
			}
			questions := make([]collaboration.Question, count)
			for i := range questions {
				questions[i] = collaboration.Question{ID: fmt.Sprintf("Q%d", i+1), Body: "Which scope boundary should apply?"}
			}
			args, _ := json.Marshal(map[string]any{"operation_id": "ask", "questions": questions, "signature": "persisted-signature"})
			scope := productionToolScope(role.Planner)
			scope.PullRequest = nil
			scope.AgentProfileName, scope.RoleDisplayName = "planner", "Planner"
			_, err = backend.Execute(context.Background(), mcp.Invocation{Name: mcp.ToolRequestClarification, Class: mcp.MutationTool, Scope: scope, OperationID: reconciliationMutationID, Arguments: args})
			if count > 5 {
				if !errors.Is(err, mcp.ErrInvalidInvocation) || ledger.round.ID != "" || ledger.published {
					t.Fatalf("oversized round admitted: %+v %v", ledger, err)
				}
			} else if err != nil || !ledger.published {
				t.Fatalf("bounded round rejected: %+v %v", ledger, err)
			}
		})
	}
}

func (f *clarificationLedger) SaveCollaborationArtifact(_ context.Context, _ store.AgentTurnLease, id string, artifact collaboration.RenderedPublication) error {
	f.round.Artifact = &artifact
	return nil
}

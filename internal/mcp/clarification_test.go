package mcp_test

import (
	"context"
	"encoding/json"
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
	round     collaboration.Round
	published bool
	source    collaboration.Source
}

func (f *clarificationLedger) RequestClarification(_ context.Context, _ store.AgentTurnLease, id string, questions []collaboration.Question) (collaboration.Round, error) {
	f.round = collaboration.Round{ID: "R" + id, Questions: questions}
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
func (f *clarificationLedger) ResolveClarification(context.Context, store.AgentTurnLease, string, []collaboration.Resolution) error {
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
			if !ledger.published || !strings.Contains(string(result), "CLARIFICATION_WAIT") || !strings.Contains(api.issueCommentRequest.Body, "persisted-signature") || !strings.Contains(api.issueCommentRequest.Body, ledger.round.ID) {
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

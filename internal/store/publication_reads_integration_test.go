//go:build integration

package store_test

import (
	"context"
	"fmt"
	"github.com/jozala/omnigrex/internal/workflow"
	"strings"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestGatewayReadsDurablePublishedHeadWithoutRebindingAuthority(t *testing.T) {
	for _, existingPR := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing_PR_%t", existingPR), func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			fixture := seedAgentSession(t, pool, 115)
			_, lease, execution := acquireReplayGatewayTurn(t, database, pool, fixture, store.AgentTurn{}, "publication-reads")
			const base = "0123456789abcdef0123456789abcdef01234567"
			const first = "1123456789abcdef0123456789abcdef01234567"
			const final = "2123456789abcdef0123456789abcdef01234567"
			publisher := &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: first, Changed: true}, {Head: final, Changed: true}}}
			api := &publicationReadGitHub{replayGatewayGitHub: &replayGatewayGitHub{head: base, branch: "omnigrex/issue-115", base: "main"}}
			newBackend := func() *mcp.ProductionBackend {
				backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: replayGatewayCredentials{}, Publisher: publisher, Workflow: &replayGatewayWorkflow{}})
				if err != nil {
					t.Fatal(err)
				}
				return backend
			}
			backend := newBackend()
			scope := replayGatewayScope(lease, execution, base)
			if existingPR {
				scope.PullRequest = &mcp.PullRequestScope{ID: 654, Number: 23}
			}
			gateway, registration := openReplayGateway(t, database, backend, scope)
			callReplayGatewayTool(t, gateway, registration, mcp.ToolGetPullRequest, map[string]any{}, !existingPR)
			callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1"}, false)
			api.head = first
			assertHead := func(name, head string, args map[string]any) {
				t.Helper()
				response := callReplayGatewayTool(t, gateway, registration, name, args, false)
				if !strings.Contains(response, head) {
					t.Fatalf("%s did not identify queried head %s: %s", name, head, response)
				}
			}
			assertHead(mcp.ToolGetCheckRuns, first, map[string]any{})
			if !existingPR {
				callReplayGatewayTool(t, gateway, registration, mcp.ToolOpenPR, map[string]any{"operation_id": "open", "title": "Changes", "body": "Ready"}, false)
			}
			assertHead(mcp.ToolGetPullRequest, first, map[string]any{})
			// Duplicate mutation delivery must not duplicate read evidence or move its target.
			callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1"}, false)
			callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-2"}, false)
			api.head = final
			assertHead(mcp.ToolGetCheckRuns, final, map[string]any{})
			assertHead(mcp.ToolGetPullRequest, final, map[string]any{})
			// A new backend and registration must reconstruct reads from PostgreSQL,
			// without replaying any mutation or depending on the publication maps.
			if err := gateway.CloseAndDrain(context.Background(), registration); err != nil {
				t.Fatal(err)
			}
			gateway, registration = openReplayGateway(t, database, newBackend(), scope)
			assertHead(mcp.ToolGetPullRequest, final, map[string]any{})
			assertHead(mcp.ToolGetCheckRuns, final, map[string]any{})
			assertHead(mcp.ToolListCIRuns, final, map[string]any{})
			assertHead(mcp.ToolGetCheckRunDiagnostics, final, map[string]any{"check_run_id": 501})
			assertHead(mcp.ToolGetCIRun, final, map[string]any{"run_id": 601})
			assertHead(mcp.ToolGetCIJobLogs, final, map[string]any{"job_id": 701})
			assertHead(mcp.ToolSearchCIJobLogs, final, map[string]any{"job_id": 701, "query": "FAIL"})
			callReplayGatewayTool(t, gateway, registration, mcp.ToolGetCIJobLogs, map[string]any{"job_id": 901}, true)
			callReplayGatewayTool(t, gateway, registration, mcp.ToolSearchCIJobLogs, map[string]any{"job_id": 901, "query": "FAIL"}, true)
			assertHead(mcp.ToolListPullRequestReviews, final, map[string]any{})
			assertHead(mcp.ToolListReviewThreads, final, map[string]any{})
			// The authority and expected-head fence remain the starting snapshot.
			if scope.HeadSHA != base || !existingPR && scope.PullRequest != nil {
				t.Fatalf("starting scope changed: %#v", scope)
			}
			wantEvidence := 3
			if existingPR {
				wantEvidence = 2
			}
			evidence, err := database.ListAgentTurnPublicationReads(context.Background(), lease, true)
			if err != nil || len(evidence) != wantEvidence {
				t.Fatalf("evidence = %#v, %v", evidence, err)
			}
			badLease := lease
			badLease.ExecutionEpoch++
			if _, err := database.ListAgentTurnPublicationReads(context.Background(), badLease, true); err == nil {
				t.Fatal("accepted another epoch")
			}
			// An unrelated external tip cannot replace the published read target.
			api.head = "3123456789abcdef0123456789abcdef01234567"
			callReplayGatewayTool(t, gateway, registration, mcp.ToolGetPullRequest, map[string]any{}, true)
			callReplayGatewayTool(t, gateway, registration, mcp.ToolListReviewThreads, map[string]any{}, true)
			if publisher.callCount() != 2 {
				t.Fatalf("publish calls = %d", publisher.callCount())
			}
		})
	}
}

func TestGatewayReadsAfterExistingPRRetryRepairsHeadAndReplaysPublication(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 116)
	ctx := context.Background()
	const base = "0123456789abcdef0123456789abcdef01234567"
	const first = "1123456789abcdef0123456789abcdef01234567"
	const final = "2123456789abcdef0123456789abcdef01234567"
	proposalID := seedFixtureChangeProposal(t, pool, ctx, fixture.workflowID, base)
	if _, err := pool.Exec(ctx, `UPDATE change_proposals SET pull_request_id=654,pull_request_number=23,head_ref='omnigrex/issue-116' WHERE id=$1`, proposalID); err != nil {
		t.Fatal(err)
	}
	api := &publicationReadGitHub{&replayGatewayGitHub{head: base, branch: "omnigrex/issue-116", base: "main"}}
	publisher := &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: first, Changed: true}, {Head: final, Changed: true}}}
	newBackend := func() *mcp.ProductionBackend {
		backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: replayGatewayCredentials{}, Publisher: publisher, Workflow: &replayGatewayWorkflow{}})
		if err != nil {
			t.Fatal(err)
		}
		return backend
	}
	// Use production verified preparation, including its Change Proposal head
	// repair, rather than just supplying an advanced head to a token fixture.
	start := func(retryOf store.AgentTurn, verified string) (store.AgentTurn, store.AgentTurnLease, store.AgentTurnExecutionContext) {
		spec := fixture.turnSpec()
		spec.ExpectedHeadSHA = base
		spec.Purpose = workflow.TurnPurposeRequestedChanges
		if retryOf.ID != "" {
			spec.Purpose = workflow.TurnPurposeRetry
			spec.RetryOfTurnID = retryOf.ID
		}
		preparationLease, preparation, err := claimFixtureAgentTurnPreparation(t, database, pool, ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		preparation.VerifiedHeadSHA, preparation.VerifiedPRID, preparation.VerifiedPRNumber = verified, 654, 23
		prepared, err := database.PrepareAgentTurn(ctx, preparationLease, preparation)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := acquireFixtureAgentTurn(t, database, pool, ctx, agentTurnExecutionJob(t, pool, ctx, prepared.Turn), prepared.Turn.ControlRevision, "existing-pr-retry", time.Minute, 1)
		if err != nil {
			t.Fatal(err)
		}
		execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.OpenMutationAdmission(ctx, lease); err != nil {
			t.Fatal(err)
		}
		return prepared.Turn, lease, execution
	}
	scopeFor := func(lease store.AgentTurnLease, execution store.AgentTurnExecutionContext, head string) mcp.TokenScope {
		scope := replayGatewayScope(lease, execution, head)
		scope.PullRequest = &mcp.PullRequestScope{ID: 654, Number: 23}
		return scope
	}
	root, lease, execution := start(store.AgentTurn{}, base)
	gateway, registration := openReplayGateway(t, database, newBackend(), scopeFor(lease, execution, base))
	callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1"}, false)
	api.head = first
	if err := gateway.CloseAndDrain(ctx, registration); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err := database.FinalizeAgentTurn(ctx, lease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "prompt interrupted after publication"}); err != nil {
		t.Fatal(err)
	}
	retry, lease, execution := start(root, first)
	if retry.ExpectedHeadSHA != first || execution.ChangeProposal == nil || execution.ChangeProposal.HeadSHA != first {
		t.Fatalf("head repair did not occur: %#v, %#v", retry, execution.ChangeProposal)
	}
	scope := scopeFor(lease, execution, first)
	gateway, registration = openReplayGateway(t, database, newBackend(), scope)
	callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1"}, false)
	evidence, err := database.ListAgentTurnPublicationReads(ctx, lease, true)
	if err != nil || len(evidence) != 1 || evidence[0].ExpectedSHA != base {
		t.Fatalf("acknowledged historical evidence = %#v, %v", evidence, err)
	}
	assertReads := func(head string) {
		t.Helper()
		for name, args := range map[string]map[string]any{
			mcp.ToolGetPullRequest: {}, mcp.ToolListPullRequestReviews: {}, mcp.ToolListReviewThreads: {}, mcp.ToolGetCheckRuns: {}, mcp.ToolListCIRuns: {},
			mcp.ToolGetCheckRunDiagnostics: {"check_run_id": 501}, mcp.ToolGetCIRun: {"run_id": 601}, mcp.ToolGetCIJobLogs: {"job_id": 701}, mcp.ToolSearchCIJobLogs: {"job_id": 701, "query": "FAIL"},
		} {
			response := callReplayGatewayTool(t, gateway, registration, name, args, false)
			if !strings.Contains(response, head) {
				t.Fatalf("%s did not query %s: %s", name, head, response)
			}
		}
	}
	assertReads(first)
	callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-2"}, false)
	api.head = final
	assertReads(final)
	if publisher.callCount() != 2 || publisher.calls[1].ExpectedOldHead != first {
		t.Fatalf("publication fence changed: %#v", publisher.calls)
	}
	if err := gateway.CloseAndDrain(ctx, registration); err != nil {
		t.Fatal(err)
	}
	gateway, registration = openReplayGateway(t, database, newBackend(), scope)
	assertReads(final)
	if scope.HeadSHA != first {
		t.Fatal("starting authority changed")
	}
	if err := gateway.CloseAndDrain(ctx, registration); err != nil {
		t.Fatal(err)
	}
}

type publicationReadGitHub struct{ *replayGatewayGitHub }

func (api *publicationReadGitHub) GetCheckRuns(_ context.Context, _, _, _, head string) ([]githubapi.CheckRun, error) {
	return []githubapi.CheckRun{{ID: 501, HeadSHA: head}}, nil
}
func (api *publicationReadGitHub) ListWorkflowRunsForHead(_ context.Context, _, _, _, head, _ string, _ int64) (githubapi.CIRunList, error) {
	return githubapi.CIRunList{HeadSHA: head}, nil
}
func (api *publicationReadGitHub) GetCheckRunDiagnostics(_ context.Context, _, _, _ string, id int64, _ string, _ int64, head string) (githubapi.CheckDiagnostics, error) {
	return githubapi.CheckDiagnostics{CheckID: id, HeadSHA: head}, nil
}
func (api *publicationReadGitHub) GetWorkflowRunDetail(_ context.Context, _, _, _ string, id int64, attempt int, _ string, _ int64, head string) (githubapi.CIRunDetail, error) {
	return githubapi.CIRunDetail{Run: githubapi.CIRun{ID: id, HeadSHA: head, RunAttempt: attempt}}, nil
}
func (api *publicationReadGitHub) ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error) {
	return []githubapi.Review{}, nil
}
func (api *publicationReadGitHub) ListReviewThreads(context.Context, string, string, string, int) ([]githubapi.ReviewThread, error) {
	return []githubapi.ReviewThread{}, nil
}

func (api *publicationReadGitHub) GetCIJob(_ context.Context, _, _, _ string, id int64) (githubapi.CIJob, error) {
	head := api.head
	if id == 901 {
		head = "0123456789abcdef0123456789abcdef01234567"
	}
	return githubapi.CIJob{ID: id, RunID: 601, RunAttempt: 1, HeadSHA: head, Status: "completed"}, nil
}
func (api *publicationReadGitHub) GetCIJobLogExcerpt(_ context.Context, _, _, _ string, scope githubapi.JobScope, _ string, _ int, _ int64) (githubapi.CILogExcerpt, error) {
	return githubapi.CILogExcerpt{HeadSHA: scope.HeadSHA, JobID: scope.ID}, nil
}
func (api *publicationReadGitHub) SearchCIJobLogs(_ context.Context, _, _, _ string, scope githubapi.JobScope, query string, _ int, _ string, _ int64) (githubapi.CILogSearchResult, error) {
	return githubapi.CILogSearchResult{HeadSHA: scope.HeadSHA, JobID: scope.ID, Query: query}, nil
}

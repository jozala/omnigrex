//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/workflow"
)

// A normally admitted Reviewer gets the richer existing read response without a
// gate record, scheduling changes or a new tool. Reconstructing the gateway must
// preserve immutable authority and reject stale/foreign continuations.
func TestGatewayReviewerCIEvidenceWithoutAdmissionGate(t *testing.T) {
	stores, pool := openPhaseFiveStores(t, 1)
	database := stores[0]
	ctx := context.Background()
	const head = "1123456789abcdef0123456789abcdef01234567"
	_, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 122, workflow.RoleReviewer, head)
	execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	api := &pgEvidenceGitHub{signedReviewerGitHub: &signedReviewerGitHub{proposal: proposal, head: head}, repoID: execution.Repository.ID, actorID: 12202}
	newBackend := func() *mcp.ProductionBackend {
		backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: replayGatewayCredentials{}, Publisher: &replayGatewayPublisher{}, Workflow: &replayGatewayWorkflow{}})
		if err != nil {
			t.Fatal(err)
		}
		return backend
	}
	scope := mcp.TokenScope{Lease: lease, WorkflowID: execution.WorkflowID, Role: workflow.RoleReviewer, Repository: mcp.RepositoryScope{ID: execution.Repository.ID, Owner: execution.Repository.Owner, Name: execution.Repository.Name}, Issue: mcp.IssueScope{ID: execution.Issue.ID, Number: execution.Issue.Number}, PullRequest: &mcp.PullRequestScope{ID: proposal.PullRequestID, Number: proposal.PullRequestNumber}, Branch: proposal.HeadRef, DefaultBranch: proposal.BaseRef, HeadSHA: head, ExpiresAt: lease.LeaseExpiresAt.Add(-time.Second)}
	gateway, registration := openReplayGateway(t, database, newBackend(), scope)
	response := callReplayGatewayTool(t, gateway, registration, mcp.ToolGetCheckRuns, map[string]any{}, false)
	for _, text := range []string{`required_policy`, `statuses`, `test_merge_sha`, `terminal_non_success`, `missing`} {
		if !strings.Contains(response, text) {
			t.Fatalf("missing %s in %s", text, response)
		}
	}
	// Parse the MCP text content, not a test-only backend result.
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(response), &rpc) != nil || len(rpc.Result.Content) == 0 {
		t.Fatalf("invalid response %s", response)
	}
	var first githubapi.CIEvidencePage
	if json.Unmarshal([]byte(rpc.Result.Content[0].Text), &first) != nil || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("missing continuation %s", response)
	}
	if err := gateway.CloseAndDrain(ctx, registration); err != nil {
		t.Fatal(err)
	}
	gateway, registration = openReplayGateway(t, database, newBackend(), scope)
	callReplayGatewayTool(t, gateway, registration, mcp.ToolGetCheckRuns, map[string]any{"cursor": first.NextCursor}, false)
	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{mcp.ToolGetCheckRunDiagnostics, map[string]any{"check_run_id": 501}}, {mcp.ToolGetCIRun, map[string]any{"run_id": 601}}, {mcp.ToolGetCIJobLogs, map[string]any{"job_id": 701}}, {mcp.ToolSearchCIJobLogs, map[string]any{"job_id": 701, "query": "failure"}},
	} {
		response := callReplayGatewayTool(t, gateway, registration, tool.name, tool.args, false)
		if !strings.Contains(response, pgEvidenceMerge) {
			t.Fatalf("lost actual CI target: %s", response)
		}
	}
	params, _ := json.Marshal(map[string]any{"name": mcp.ToolGetCheckRuns, "arguments": map[string]any{"head_sha": pgEvidenceMerge}})
	rejected := replayGatewayRPC(t, gateway, registration, mcp.ProtocolVersion, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":%s}`, params))
	if !strings.Contains(rejected, `"error"`) {
		t.Fatalf("caller-selected SHA accepted: %s", rejected)
	}
	callReplayGatewayTool(t, gateway, registration, mcp.ToolGetCheckRunDiagnostics, map[string]any{"check_run_id": 999}, true)
	api.stale = true
	callReplayGatewayTool(t, gateway, registration, mcp.ToolGetCheckRuns, map[string]any{"cursor": first.NextCursor}, true)
	callReplayGatewayTool(t, gateway, registration, mcp.ToolGetCheckRunDiagnostics, map[string]any{"check_run_id": 501}, true)
	if scope.HeadSHA != head || scope.PullRequest.ID != proposal.PullRequestID {
		t.Fatal("immutable authority changed")
	}
	// The verified merge candidate must not become a review mutation target.
	params, _ = json.Marshal(map[string]any{"name": mcp.ToolSubmitReview, "arguments": map[string]any{"operation_id": "reject-merge-review", "event": "APPROVE", "body": "review", "comments": []any{}, "head_sha": pgEvidenceMerge}})
	rejected = replayGatewayRPC(t, gateway, registration, mcp.ProtocolVersion, fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":%s}`, params))
	if !strings.Contains(rejected, `"error"`) && !strings.Contains(rejected, `"isError":true`) {
		t.Fatalf("caller-selected review SHA accepted: %s", rejected)
	}
	callReplayGatewayTool(t, gateway, registration, mcp.ToolSubmitReview, map[string]any{"operation_id": "review-original-head", "event": "APPROVE", "body": "Review remains on the frozen head.", "comments": []any{}}, false)
	if api.submitted == nil || api.submitted.CommitID != head {
		t.Fatalf("CI reads rebound review mutation authority: %+v", api.submitted)
	}
}

const pgEvidenceMerge = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type pgEvidenceGitHub struct {
	*signedReviewerGitHub
	repoID  int64
	actorID int64
	stale   bool
}

func (g *pgEvidenceGitHub) DiscoverRequiredCI(context.Context, string, string, string, string) githubapi.CIInventory {
	return githubapi.CIInventory{Availability: githubapi.CIAvailable, Requirements: []githubapi.CIRequirement{{Context: "build", Source: "classic"}, {Context: "missing", Source: "classic"}}}
}
func (g *pgEvidenceGitHub) VerifyCITarget(_ context.Context, _, _, _ string, repo, pr int64, number int, head, _, _ string) githubapi.CITargetMapping {
	mapping := githubapi.CITargetMapping{RepositoryID: repo, PullRequestID: pr, PullRequestNumber: number, ReviewedHead: head, State: "verified", TestMergeSHA: pgEvidenceMerge, BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", BaseRef: g.proposal.BaseRef}
	if g.stale {
		mapping.State = "stale"
		mapping.TestMergeSHA = ""
	}
	return mapping
}
func (g *pgEvidenceGitHub) ObserveCI(_ context.Context, _, _, _, head string, repo int64) githubapi.CIObservation {
	idOffset := int64(0)
	if head == g.head {
		idOffset = 1000
	}
	observed := githubapi.CIObservation{Availability: githubapi.CIAvailable, RepositoryID: repo, HeadSHA: head, Statuses: []githubapi.CommitStatus{{ID: 1 + idOffset, Context: "build", State: "failure"}}}
	for i := 0; i < 45; i++ {
		observed.Checks = append(observed.Checks, githubapi.CheckRun{ID: int64(501+i) + idOffset, Name: fmt.Sprintf("check-%d", i), HeadSHA: head, Status: "queued", App: &githubapi.CheckApp{ID: 7}})
	}
	return observed
}
func (g *pgEvidenceGitHub) GetCheckRunMetadata(_ context.Context, _, _, _ string, id int64) (githubapi.CheckRun, error) {
	head := pgEvidenceMerge
	if id == 999 {
		head = "cccccccccccccccccccccccccccccccccccccccc"
	}
	return githubapi.CheckRun{ID: id, HeadSHA: head}, nil
}
func (g *pgEvidenceGitHub) GetCheckRunDiagnostics(_ context.Context, _, _, _ string, id int64, _ string, _ int64, head string) (githubapi.CheckDiagnostics, error) {
	return githubapi.CheckDiagnostics{CheckID: id, HeadSHA: head}, nil
}
func (g *pgEvidenceGitHub) GetWorkflowRun(_ context.Context, _, _, _ string, id int64) (githubapi.CIRun, error) {
	return githubapi.CIRun{ID: id, HeadSHA: pgEvidenceMerge, RepositoryID: g.repoID, Event: "pull_request", RunAttempt: 1}, nil
}
func (g *pgEvidenceGitHub) GetWorkflowRunDetail(ctx context.Context, token, owner, repo string, id int64, _ int, _ string, _ int64, _ string) (githubapi.CIRunDetail, error) {
	run, err := g.GetWorkflowRun(ctx, token, owner, repo, id)
	return githubapi.CIRunDetail{Run: run}, err
}
func (g *pgEvidenceGitHub) GetCIJob(_ context.Context, _, _, _ string, id int64) (githubapi.CIJob, error) {
	return githubapi.CIJob{ID: id, RunID: 601, RunAttempt: 1, HeadSHA: pgEvidenceMerge}, nil
}
func (g *pgEvidenceGitHub) GetCIJobLogExcerpt(_ context.Context, _, _, _ string, scope githubapi.JobScope, _ string, _ int, _ int64) (githubapi.CILogExcerpt, error) {
	return githubapi.CILogExcerpt{HeadSHA: scope.HeadSHA, JobID: scope.ID}, nil
}
func (g *pgEvidenceGitHub) SearchCIJobLogs(_ context.Context, _, _, _ string, scope githubapi.JobScope, query string, _ int, _ string, _ int64) (githubapi.CILogSearchResult, error) {
	return githubapi.CILogSearchResult{HeadSHA: scope.HeadSHA, JobID: scope.ID, Query: query}, nil
}

func (g *pgEvidenceGitHub) SubmitReview(ctx context.Context, token, owner, repo string, number int, request githubapi.ReviewRequest) (githubapi.Review, error) {
	review, err := g.signedReviewerGitHub.SubmitReview(ctx, token, owner, repo, number, request)
	if err != nil {
		return review, err
	}
	g.mutex.Lock()
	defer g.mutex.Unlock()
	review.User.ID = g.actorID
	g.submitted = &review
	return review, nil
}

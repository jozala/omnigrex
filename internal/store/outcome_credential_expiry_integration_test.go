//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

// expiringReconcilerGitHub rejects the stale Turn-start snapshot with HTTP 401
// while serving the current installation token, proving final observations do
// not use the expired snapshot.
type expiringReconcilerGitHub struct {
	mutex       sync.Mutex
	pullRequest githubapi.PullRequest
	seen        []string
	getCalls    int
}

func (github *expiringReconcilerGitHub) GetPullRequest(_ context.Context, credential, _, _ string, _ int) (githubapi.PullRequest, error) {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	github.seen = append(github.seen, credential)
	github.getCalls++
	if credential == "stale-developer-token" {
		return githubapi.PullRequest{}, &githubapi.APIError{StatusCode: 401, Message: "bad credentials"}
	}
	return github.pullRequest, nil
}

func (github *expiringReconcilerGitHub) ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error) {
	return nil, fmt.Errorf("unexpected Pull Request review read in Developer handoff")
}

func (github *expiringReconcilerGitHub) sawOnly(token string) bool {
	github.mutex.Lock()
	defer github.mutex.Unlock()
	if len(github.seen) == 0 {
		return false
	}
	for _, seen := range github.seen {
		if seen != token {
			return false
		}
	}
	return true
}

// TestStaleSnapshotReconcilesWithCurrentCredentialExactlyOnce proves a Turn
// that outlives its Turn-start token still settles directly using a current
// credential: exactly one settlement and successor, with no deferred
// corroboration checkpoint or recovery job.
func TestStaleSnapshotReconcilesWithCurrentCredentialExactlyOnce(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 98, workflow.RoleDeveloper, "")
	const head = "3123456789abcdef0123456789abcdef01234567"
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "request-review-expiry", ToolName: mcp.ToolRequestReview,
		Request:         json.RawMessage(`{"operation_id":"request-review-expiry","summary":"Ready for review"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:feature", proposal.RepositoryID), ExpectedSHA: head,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]any{
		"outcome": "REVIEW_REQUESTED", "pull_request_id": proposal.PullRequestID,
		"pull_request_number": proposal.PullRequestNumber, "head_sha": head,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	github := &expiringReconcilerGitHub{pullRequest: githubapi.PullRequest{
		ID: proposal.PullRequestID, NodeID: proposal.PullRequestNodeID, Number: int(proposal.PullRequestNumber), State: "open",
		Head: githubapi.PullRequestBranch{Ref: proposal.HeadRef, SHA: head, Label: "owner:feature"},
		Base: githubapi.PullRequestBranch{Ref: proposal.BaseRef, SHA: proposal.BaseSHA, Label: "owner:main"},
	}}
	developer := &integrationCredentialProvider{credential: "installation-token"}
	reviewer := &integrationCredentialProvider{credential: "installation-token"}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{
		Store: database, GitHub: github,
		DeveloperCredentials: developer, ReviewerCredentials: reviewer,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := agentturn.OutcomeReconciliation{
		Lease: lease, Execution: execution, PromptResponse: &acp.PromptResponse{StopReason: acp.StopReasonEndTurn},
		// The expired Turn-start snapshot must never reach GitHub.
		RepositoryCredential: "stale-developer-token", Paths: committedPublicationPaths(t),
	}
	var failure *agentturn.TerminalCorroborationFailure
	request.OnCorroborationFailure = func(observed agentturn.TerminalCorroborationFailure) { failure = &observed }
	observation, err := reconciler.Reconcile(ctx, request)
	if err != nil || observation.Outcome != workflow.TurnOutcomeChangeProposalReady || observation.Completion.Status != store.AgentTurnSucceeded ||
		observation.ChangeProposal == nil || observation.ChangeProposal.HeadSHA != head {
		t.Fatalf("stale-snapshot handoff observation = (%#v, %v)", observation, err)
	}
	if failure != nil {
		t.Fatalf("successful intent took a recovery detour: %#v", failure)
	}
	if !github.sawOnly("installation-token") {
		t.Fatalf("final observations did not use only the current credential: %q", github.seen)
	}
	settled, err := database.SettleAgentTurn(ctx, lease, observation)
	if err != nil || settled.State != workflow.StateReviewing || settled.SuccessorJobID == "" {
		t.Fatalf("stale-snapshot settlement = (%#v, %v)", settled, err)
	}
	var settlements, successors, checkpoints, verifierJobs int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_turn_settlements WHERE agent_turn_id = $1),
       (SELECT count(*) FROM jobs AS successor JOIN agent_turn_settlements AS settlement
            ON successor.agent_turn_settlement_id = settlement.id
            WHERE settlement.agent_turn_id = $1 AND successor.kind = 'PREPARE_AGENT_TURN'),
       (SELECT count(*) FROM agent_turn_corroborations WHERE agent_turn_id = $1 AND state = 'PENDING'),
       (SELECT count(*) FROM jobs WHERE agent_turn_id = $1 AND kind = 'VERIFY_TERMINAL_INTENT' AND status = 'AVAILABLE')`,
		lease.ID).Scan(&settlements, &successors, &checkpoints, &verifierJobs); err != nil {
		t.Fatal(err)
	}
	if settlements != 1 || successors != 1 || checkpoints != 0 || verifierJobs != 0 {
		t.Fatalf("stale-snapshot effects = settlements %d, successors %d, checkpoints %d, verifier jobs %d; want 1, 1, 0, 0",
			settlements, successors, checkpoints, verifierJobs)
	}
}

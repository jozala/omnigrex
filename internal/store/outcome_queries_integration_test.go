//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
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

func TestListAgentTurnMutationInvocationsRequiresClosedSettledLedgerAndPreservesOrder(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 91)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	turn, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	job := claimAgentTurnJob(t, database, ctx, turn, time.Second)
	lease, err := database.AcquireAgentTurn(ctx, job, turn.ControlRevision, "outcome-query", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	first, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "first", ToolName: "comment_on_issue", Request: json.RawMessage(`{"body":"first"}`)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "second", ToolName: "comment_on_issue", Request: json.RawMessage(`{"body":"second"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ListAgentTurnMutationInvocations(ctx, lease); !errors.Is(err, store.ErrMutationAdmissionClosed) {
		t.Fatalf("ListAgentTurnMutationInvocations() before close error = %v, want ErrMutationAdmissionClosed", err)
	}
	if _, err := database.StartMutation(ctx, lease, second.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, second.ID, json.RawMessage(`{"comment_id":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ListAgentTurnMutationInvocations(ctx, lease); !errors.Is(err, store.ErrAgentTurnMutationsUnsettled) {
		t.Fatalf("ListAgentTurnMutationInvocations() with reservation error = %v, want ErrAgentTurnMutationsUnsettled", err)
	}
	if err := database.FailMutation(ctx, lease, first.ID, errors.New("denied")); err != nil {
		t.Fatal(err)
	}
	mutations, err := database.ListAgentTurnMutationInvocations(ctx, lease)
	if err != nil {
		t.Fatalf("ListAgentTurnMutationInvocations() error = %v", err)
	}
	if len(mutations) != 2 || mutations[0].ID != first.ID || mutations[0].State != store.MutationFailed ||
		mutations[1].ID != second.ID || mutations[1].State != store.MutationSucceeded {
		t.Fatalf("ordered terminal mutations = %#v", mutations)
	}
}

func TestRetryMutationReplayIsExplicitOrderedAndVisibleToOutcomeReconciliation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 4)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 93)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	root, err := database.AllocateAgentTurn(ctx, fixture.turnSpec())
	if err != nil {
		t.Fatal(err)
	}
	rootLease, err := database.AcquireAgentTurn(ctx, claimAgentTurnJob(t, database, ctx, root, time.Second), root.ControlRevision, "replay-root", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.OpenMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	terminalSpec := store.MutationSpec{
		OperationID: "terminal-blocked", ToolName: mcp.ToolReportBlocked,
		Request:         json.RawMessage(`{"operation_id":"terminal-blocked","reason":"dependency unavailable","details":"retry replay"}`),
		ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID,
	}
	terminal, err := database.ReserveMutation(ctx, rootLease, terminalSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, rootLease, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, rootLease, terminal.ID, json.RawMessage(`{"outcome":"BLOCKED","reason":"dependency unavailable","details":"retry replay"}`)); err != nil {
		t.Fatal(err)
	}
	failedSpec := store.MutationSpec{OperationID: "failed-source", ToolName: "comment_on_issue", Request: json.RawMessage(`{"body":"denied"}`)}
	failed, err := database.ReserveMutation(ctx, rootLease, failedSpec)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.FailMutation(ctx, rootLease, failed.ID, errors.New("permission denied")); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, rootLease); err != nil {
		t.Fatal(err)
	}
	if err := database.FinalizeAgentTurn(ctx, rootLease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "infrastructure failure"}); err != nil {
		t.Fatal(err)
	}

	acquireRetry := func(target store.AgentTurn, owner string) (store.AgentTurn, store.AgentTurnLease) {
		t.Helper()
		spec := fixture.turnSpec()
		spec.Purpose = workflow.TurnPurposeRetry
		spec.RetryOfTurnID = target.ID
		turn, err := database.AllocateAgentTurn(ctx, spec)
		if err != nil {
			t.Fatalf("AllocateAgentTurn() retry error = %v", err)
		}
		lease, err := database.AcquireAgentTurn(ctx, claimAgentTurnJob(t, database, ctx, turn, time.Second), turn.ControlRevision, owner, time.Second, 1)
		if err != nil {
			t.Fatalf("AcquireAgentTurn() retry error = %v", err)
		}
		if err := database.OpenMutationAdmission(ctx, lease); err != nil {
			t.Fatal(err)
		}
		return turn, lease
	}

	firstRetry, retryLease := acquireRetry(root, "replay-retry")
	execution, err := database.GetAgentTurnExecutionContext(ctx, retryLease)
	if err != nil {
		t.Fatal(err)
	}
	directBefore, err := database.ReserveMutation(ctx, retryLease, store.MutationSpec{OperationID: "direct-before", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.FailMutation(ctx, retryLease, directBefore.ID, errors.New("before failed")); err != nil {
		t.Fatal(err)
	}

	const callers = 12
	reservations := make(chan store.MutationReservation, callers)
	errorsByCall := make(chan error, callers)
	var wait sync.WaitGroup
	for index := range callers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			reservation, err := databases[index%len(databases)].ReserveMutation(ctx, retryLease, terminalSpec)
			reservations <- reservation
			errorsByCall <- err
		}(index)
	}
	wait.Wait()
	close(reservations)
	close(errorsByCall)
	for err := range errorsByCall {
		if err != nil {
			t.Fatalf("concurrent cached ReserveMutation() error = %v", err)
		}
	}
	for reservation := range reservations {
		if reservation.ID != terminal.ID || reservation.AgentTurnID != root.ID || reservation.State != store.MutationSucceeded {
			t.Fatalf("concurrent cached reservation = %#v, want source %s", reservation, terminal.ID)
		}
	}
	var unacknowledged int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM tool_invocation_replays WHERE agent_turn_id = $1`, retryLease.ID).Scan(&unacknowledged); err != nil {
		t.Fatal(err)
	}
	if unacknowledged != 0 {
		t.Fatalf("ReserveMutation() recorded %d replay rows before acknowledgement", unacknowledged)
	}
	if err := database.AcknowledgeMutationReplay(ctx, retryLease, terminal.ID, terminalSpec); err != nil {
		t.Fatalf("AcknowledgeMutationReplay() terminal error = %v", err)
	}
	if repeated, err := database.ReserveMutation(ctx, retryLease, terminalSpec); err != nil || repeated.ID != terminal.ID {
		t.Fatalf("repeated replay ReserveMutation() = (%#v, %v)", repeated, err)
	}
	if replayedFailure, err := database.ReserveMutation(ctx, retryLease, failedSpec); err != nil || replayedFailure.ID != failed.ID || replayedFailure.State != store.MutationFailed {
		t.Fatalf("failed replay ReserveMutation() = (%#v, %v)", replayedFailure, err)
	}
	if err := database.AcknowledgeMutationReplay(ctx, retryLease, failed.ID, failedSpec); err != nil {
		t.Fatalf("AcknowledgeMutationReplay() failure error = %v", err)
	}
	conflict := terminalSpec
	conflict.Request = json.RawMessage(`{"operation_id":"terminal-blocked","reason":"different","details":"retry replay"}`)
	if _, err := database.ReserveMutation(ctx, retryLease, conflict); !errors.Is(err, store.ErrMutationOperationConflict) {
		t.Fatalf("conflicting replay definition error = %v, want ErrMutationOperationConflict", err)
	}
	malformed := terminalSpec
	malformed.Request = json.RawMessage(`{"operation_id":`)
	if _, err := database.ReserveMutation(ctx, retryLease, malformed); err == nil {
		t.Fatal("malformed replay definition succeeded")
	}
	directAfter, err := database.ReserveMutation(ctx, retryLease, store.MutationSpec{OperationID: "direct-after", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.FailMutation(ctx, retryLease, directAfter.ID, errors.New("after failed")); err != nil {
		t.Fatal(err)
	}

	var replayCount int
	var nextInvocation int64
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM tool_invocation_replays WHERE agent_turn_id = $1),
       (SELECT next_invocation_number FROM agent_turns WHERE id = $1)`, retryLease.ID).Scan(&replayCount, &nextInvocation); err != nil {
		t.Fatal(err)
	}
	if replayCount != 2 || nextInvocation != 5 {
		t.Fatalf("durable replay accounting = %d rows, next invocation %d; want 2 and 5", replayCount, nextInvocation)
	}
	if err := database.CloseMutationAdmission(ctx, retryLease); err != nil {
		t.Fatal(err)
	}
	ledger, err := database.ListAgentTurnMutationInvocations(ctx, retryLease)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{directBefore.ID, terminal.ID, failed.ID, directAfter.ID}
	wantStates := []store.MutationState{store.MutationFailed, store.MutationSucceeded, store.MutationFailed, store.MutationFailed}
	if len(ledger) != len(wantIDs) {
		t.Fatalf("retry terminal ledger = %#v", ledger)
	}
	for index, mutation := range ledger {
		if mutation.ID != wantIDs[index] || mutation.State != wantStates[index] || mutation.InvocationNumber != int64(index+1) ||
			mutation.AgentTurnID != retryLease.ID || mutation.ExecutionEpoch != retryLease.ExecutionEpoch {
			t.Errorf("retry terminal ledger[%d] = %#v", index, mutation)
		}
	}

	github := &replayOutcomeGitHub{}
	reconciler, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: database, GitHub: github})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := reconciler.Reconcile(ctx, agentturn.OutcomeReconciliation{
		Lease: retryLease, Execution: execution,
		PromptResponse: &acp.PromptResponse{StopReason: acp.StopReasonEndTurn},
	})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Outcome != workflow.TurnOutcomeBlocked || observation.Completion.Status != store.AgentTurnSucceeded ||
		observation.Diagnostic != "dependency unavailable: retry replay" || github.calls != 0 {
		t.Fatalf("replayed terminal intent reconciliation = %#v, GitHub calls %d", observation, github.calls)
	}

	if err := database.FinalizeAgentTurn(ctx, retryLease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "second infrastructure failure"}); err != nil {
		t.Fatal(err)
	}
	_, noReplayLease := acquireRetry(firstRetry, "replay-not-called")
	noReplayExecution, err := database.GetAgentTurnExecutionContext(ctx, noReplayLease)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, noReplayLease); err != nil {
		t.Fatal(err)
	}
	noReplayLedger, err := database.ListAgentTurnMutationInvocations(ctx, noReplayLease)
	if err != nil {
		t.Fatal(err)
	}
	if len(noReplayLedger) != 0 {
		t.Fatalf("retry without replay call inherited terminal ledger = %#v", noReplayLedger)
	}
	observation, err = reconciler.Reconcile(ctx, agentturn.OutcomeReconciliation{
		Lease: noReplayLease, Execution: noReplayExecution,
		PromptResponse: &acp.PromptResponse{StopReason: acp.StopReasonEndTurn},
	})
	if err != nil || observation.Outcome != workflow.TurnOutcomeInfrastructureFailed ||
		observation.Diagnostic != "ACP prompt ended without a successful terminal mutation intent" {
		t.Fatalf("retry without replay reconciliation = (%#v, %v)", observation, err)
	}
}

type replayOutcomeGitHub struct {
	calls int
}

func (github *replayOutcomeGitHub) GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error) {
	github.calls++
	return githubapi.PullRequest{}, errors.New("unexpected GitHub call")
}

func (github *replayOutcomeGitHub) ListPullRequestReviews(context.Context, string, string, string, int) ([]githubapi.Review, error) {
	github.calls++
	return nil, errors.New("unexpected GitHub call")
}

func TestGetChangeProposalReviewReturnsDurableIdentity(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 92, workflow.RoleReviewer, "review-head")
	review := &workflow.ReviewIdentity{
		ID: 9201, NodeID: "PRR_9201", ChangeProposalID: proposal.PullRequestID,
		ActorID: 9202, HeadSHA: proposal.HeadSHA,
	}
	observation := successfulSettlementObservation(workflow.TurnOutcomeApproved, proposal)
	observation.Review = review
	observation.AuthorizedReviewerActorID = review.ActorID
	if _, err := database.SettleAgentTurn(ctx, lease, observation); err != nil {
		t.Fatalf("SettleAgentTurn() error = %v", err)
	}

	stored, err := database.GetChangeProposalReview(ctx, proposal.RepositoryID, review.ID)
	if err != nil {
		t.Fatalf("GetChangeProposalReview() error = %v", err)
	}
	if stored == nil || *stored != *review {
		t.Fatalf("GetChangeProposalReview() = %#v, want %#v", stored, review)
	}
	missing, err := database.GetChangeProposalReview(ctx, proposal.RepositoryID, review.ID+1)
	if err != nil || missing != nil {
		t.Fatalf("GetChangeProposalReview() missing = (%#v, %v), want nil", missing, err)
	}
}

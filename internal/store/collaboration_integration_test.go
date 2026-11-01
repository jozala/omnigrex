//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestClarificationDurableWaitAndInputFences(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for n, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		t.Run(string(role), func(t *testing.T) {
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 960+n, role, "current-head")
			request := json.RawMessage(`{"operation_id":"question1","questions":[{"id":"Q1","body":"Compatibility?"},{"id":"Q2","body":"Retention?"}]}`)
			mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{ToolName: "request_clarification", OperationID: "question1", Request: request, ExternalService: "github", ExternalResourceID: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = database.StartMutation(ctx, lease, mutation.ID); err != nil {
				t.Fatal(err)
			}
			round, err := database.RequestClarification(ctx, lease, mutation.ID, []collaboration.Question{{ID: "Q1", Body: "Compatibility?"}, {ID: "Q2", Body: "Retention?"}})
			if err != nil {
				t.Fatal(err)
			}
			input := store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "answer", Target: round.ID, Body: "1. Preserve compatibility. Cannot decide 2."}, Actor: collaboration.Actor{ID: "maintainer", Name: "M"}, Source: collaboration.Source{Channel: "fixture", ID: "reply-during-publication"}, Authorized: true, At: time.Now()}
			receipt, err := database.AdmitHumanInput(ctx, input)
			if err != nil || receipt.ResponseID == "" {
				t.Fatalf("admit = %+v %v", receipt, err)
			}
			replay, err := database.AdmitHumanInput(ctx, input)
			if err != nil || replay.ResponseID != receipt.ResponseID {
				t.Fatalf("replay=%+v %v", replay, err)
			}
			if err = database.ConfirmClarificationPublication(ctx, fixture.workflowID, round.ID, collaboration.Source{Channel: "fixture", ID: "published-question"}); err != nil {
				t.Fatal(err)
			}
			result, _ := json.Marshal(map[string]any{"outcome": "CLARIFICATION_WAIT", "round_id": round.ID})
			if err = database.CompleteMutation(ctx, lease, mutation.ID, result); err != nil {
				t.Fatal(err)
			}
			_, err = database.ReserveMutation(ctx, lease, store.MutationSpec{ToolName: "request_review", OperationID: "handoff", Request: json.RawMessage(`{"operation_id":"handoff","summary":"ready"}`), ExternalService: "github", ExternalResourceID: "fixture"})
			if !errors.Is(err, store.ErrPendingHumanInput) {
				t.Fatalf("pending input did not fence handoff: %v", err)
			}
			if err = database.CloseMutationAdmission(ctx, lease); err != nil {
				t.Fatal(err)
			}
			settled, err := database.SettleAgentTurn(ctx, lease, store.AgentTurnSettlementObservation{ObservedAt: time.Now(), Outcome: workflow.TurnOutcomeClarificationWait, Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded}})
			if err != nil {
				t.Fatal(err)
			}
			if settled.SuccessorJobID == "" || settled.State == workflow.StateNeedsHuman {
				t.Fatalf("wait settlement=%+v", settled)
			}
			var active bool
			var attempt string
			var failures int
			var revision int
			var stateRaw []byte
			if err = pool.QueryRow(ctx, `SELECT turn.active,attempt.id::text,attempt.infrastructure_failures,workflow.state_revision,collab.state FROM agent_turns turn JOIN workflows workflow ON workflow.id=turn.workflow_id JOIN workflow_attempts attempt ON attempt.id=turn.workflow_attempt_id JOIN work_item_collaboration collab ON collab.workflow_id=workflow.id WHERE turn.id=$1`, lease.ID).Scan(&active, &attempt, &failures, &revision, &stateRaw); err != nil {
				t.Fatal(err)
			}
			if active || attempt != fixture.attemptID || failures != 0 {
				t.Fatalf("wait reset continuity: active=%t attempt=%s retries=%d", active, attempt, failures)
			}
			var state collaboration.State
			if err = json.Unmarshal(stateRaw, &state); err != nil {
				t.Fatal(err)
			}
			if len(state.Inputs) != 1 || len(state.Rounds[0].Resolutions) != 0 || state.Inputs[0].DeliveredTo != "" {
				t.Fatalf("input state=%+v", state)
			}
			// A second accepted response coalesces onto the already queued successor.
			input.Source.ID = "second"
			input.Command.Body = "2. Seven days."
			second, err := databases[1].AdmitHumanInput(ctx, input)
			if err != nil || second.ResponseID == "" {
				t.Fatal(second, err)
			}
			var jobs int
			if err = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id=$1 AND kind='PREPARE_AGENT_TURN' AND status IN ('AVAILABLE','LEASED')`, fixture.workflowID).Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			if jobs != 1 {
				t.Fatalf("queued %d concurrent successors", jobs)
			}
			// Concurrent and redelivered responses serialize into one durable successor.
			var wg sync.WaitGroup
			outcomes := make(chan error, 4)
			for _, source := range []string{"concurrent-1", "concurrent-1", "concurrent-2", "concurrent-3"} {
				wg.Add(1)
				go func(source string) {
					defer wg.Done()
					submission := input
					submission.Source.ID = source
					submission.Command.Body = "A concurrent authorized answer."
					r, err := database.AdmitHumanInput(ctx, submission)
					if err == nil && r.ResponseID == "" {
						err = fmt.Errorf("input rejected: %s", r.Feedback)
					}
					outcomes <- err
				}(source)
			}
			wg.Wait()
			close(outcomes)
			for err := range outcomes {
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&stateRaw); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(stateRaw, &state); err != nil {
				t.Fatal(err)
			}
			if len(state.Inputs) != 5 {
				t.Fatalf("concurrent input count=%d", len(state.Inputs))
			}
			if err = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id=$1 AND kind='PREPARE_AGENT_TURN' AND status IN ('AVAILABLE','LEASED')`, fixture.workflowID).Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			if jobs != 1 {
				t.Fatalf("concurrent responses allocated %d successors", jobs)
			}
			// Lifecycle fencing cannot be bypassed by a steering command.
			if _, err = pool.Exec(ctx, `UPDATE workflows SET status='NEEDS_HUMAN' WHERE id=$1`, fixture.workflowID); err != nil {
				t.Fatal(err)
			}
			input.Source.ID = "handoff"
			input.Command = collaboration.Command{Kind: "steer", Body: "Continue anyway"}
			rejected, err := database.AdmitHumanInput(ctx, input)
			if err != nil || rejected.ResponseID != "" {
				t.Fatalf("handoff bypass=%+v %v", rejected, err)
			}
		})
	}
}

func TestHumanSteeringRoutesAfterCommittedStageBeforeParticipantAllocation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 965, workflow.RoleDeveloper, "")
	settled, err := database.SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomeChangeProposalReady, proposal))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := database.AdmitHumanInput(ctx, store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "steer", Body: "Review the compatibility edge case."}, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "after-stage-commit"}, Authorized: true, At: time.Now()})
	if err != nil || receipt.ResponseID == "" {
		t.Fatal(receipt, err)
	}
	job := claimSettlementSuccessorJob(t, database, pool, ctx, settled.SuccessorJobID, "clarification-preparation")
	prepared, err := prepareWithFixtureAdmission(t, database, ctx, *job, preparationSpec("head", "provider/test"))
	if err != nil {
		t.Fatal(err)
	}
	var configuration struct {
		Inputs []collaboration.Input `json:"human_input"`
	}
	if err = json.Unmarshal(prepared.Turn.TurnConfiguration, &configuration); err != nil {
		t.Fatal(err)
	}
	if prepared.Participant.Role != workflow.RoleReviewer || len(configuration.Inputs) != 1 || configuration.Inputs[0].ID != receipt.ResponseID || configuration.Inputs[0].DeliveredTo != prepared.Turn.ID || prepared.Turn.WorkflowAttemptID != fixture.attemptID {
		t.Fatalf("misrouted input: %+v %+v", prepared, configuration)
	}
}

func TestHumanInputAcceptedBeforeStageCommitFencesSuccessfulHandoff(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 966, workflow.RoleDeveloper, "")
	receipt, err := database.AdmitHumanInput(ctx, store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "steer", Body: "Consider compatibility before review."}, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "before-stage-commit"}, Authorized: true, At: time.Now()})
	if err != nil || receipt.ResponseID == "" {
		t.Fatal(receipt, err)
	}
	settled, err := database.SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomeChangeProposalReady, proposal))
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != workflow.StateDeveloping || settled.SuccessorJobID == "" {
		t.Fatalf("input was bypassed by handoff: %+v", settled)
	}
	job := claimSettlementSuccessorJob(t, database, pool, ctx, settled.SuccessorJobID, "input-preparation")
	spec := preparationSpec("head", "provider/test")
	updateStagePreparation(&spec, workflow.StageImplementation, func(p *store.ParticipantPreparation) {
		p.Binding.RuntimeImageDigest = "sha256:test"
		original := fixture.turnSpec()
		p.Profile = store.AgentProfileSnapshot{CommitSHA: original.AgentProfileCommitSHA, ContentSHA256: original.AgentProfileContentSHA256, Config: original.AgentProfileConfig}
	})
	prepared, err := prepareWithFixtureAdmission(t, database, ctx, *job, spec)
	if err != nil {
		t.Fatal(err)
	}
	var configuration struct {
		Inputs []collaboration.Input `json:"human_input"`
	}
	if err = json.Unmarshal(prepared.Turn.TurnConfiguration, &configuration); err != nil {
		t.Fatal(err)
	}
	if prepared.Participant.ID != lease.JobLease.AgentParticipantID || prepared.Session.ID != lease.JobLease.AgentSessionID || len(configuration.Inputs) != 1 || configuration.Inputs[0].ID != receipt.ResponseID || prepared.Turn.WorkflowAttemptID != fixture.attemptID {
		t.Fatalf("continuation did not deliver to retained agent: %+v %+v", prepared, configuration)
	}
}

func TestKnownFailedQuestionPublicationDoesNotStrandClarificationRecovery(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 967, workflow.RoleDeveloper, "")
	request := func(operation string) collaboration.Round {
		mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: operation, ToolName: "request_clarification", Request: json.RawMessage(`{"questions":[{"id":"Q1","body":"Compatibility policy?"}]}`), ExternalService: "github", ExternalResourceID: "fixture-issue"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = database.StartMutation(ctx, lease, mutation.ID); err != nil {
			t.Fatal(err)
		}
		round, err := database.RequestClarification(ctx, lease, mutation.ID, []collaboration.Question{{ID: "Q1", Body: "Compatibility policy?"}})
		if err != nil {
			t.Fatal(err)
		}
		if err = database.FailMutation(ctx, lease, mutation.ID, errors.New("publication definitively rejected")); err != nil {
			t.Fatal(err)
		}
		return round
	}
	first := request("failed-publication")
	second := request("retry-after-proven-failure")
	restored, err := database.GetClarificationRound(ctx, fixture.workflowID, first.ID)
	if err != nil || !restored.PublicationFailed || restored.Published || second.ID == first.ID {
		t.Fatalf("failed publication recovery: %+v %+v %v", restored, second, err)
	}
	if err = database.ConfirmClarificationPublication(ctx, fixture.workflowID, first.ID, collaboration.Source{Channel: "fixture", ID: "unexpected"}); err == nil {
		t.Fatal("failed publication later became current")
	}
}

//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	var applied, observed, jobOutcome string
	if err = pool.QueryRow(ctx, `SELECT workflow_outcome,observation->>'outcome',(SELECT result->>'workflow_outcome' FROM jobs WHERE id=$2) FROM agent_turn_settlements WHERE id=$1`, settled.ID, lease.JobLease.ID).Scan(&applied, &observed, &jobOutcome); err != nil {
		t.Fatal(err)
	}
	if applied != "CLARIFICATION_WAIT" || jobOutcome != applied || observed != "CHANGE_PROPOSAL_READY" {
		t.Fatalf("observation/applied provenance disagrees: %s %s %s", observed, applied, jobOutcome)
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

func TestCollaborationRecordRetrievalPreservesOversizedPreparedBatchAndScope(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, lease, _ := prepareOpenSettlementTurn(t, databases[0], pool, ctx, 9926, workflow.RoleDeveloper, "head")
	var state collaboration.State
	for i := 0; i < 20; i++ {
		id := fmt.Sprint(i)
		receipt := state.Accept("A"+id, collaboration.Command{Kind: "steer", Body: strings.Repeat("\"\\\n<界", 12000)}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: lease.AgentParticipantID, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: id}, At: time.Now()})
		if receipt.ResponseID == "" {
			t.Fatal(receipt)
		}
	}
	state.Prepare(lease.AgentParticipantID, lease.ID)
	config, _ := json.Marshal(state.Project(lease.AgentParticipantID, lease.ID))
	raw, _ := json.Marshal(state)
	if len(config) > collaboration.MaxProjectionBytes {
		t.Fatal("unbounded configuration")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2) ON CONFLICT(workflow_id) DO UPDATE SET state=EXCLUDED.state`, fixture.workflowID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_turns SET turn_configuration=$2 WHERE id=$1`, lease.ID, config); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 20; index++ {
		var content strings.Builder
		offset := 0
		for {
			page, err := databases[0].GetCollaborationRecord(ctx, fixture.workflowID, lease.AgentParticipantID, lease.ID, "human_input", index, offset)
			if err != nil {
				t.Fatal(err)
			}
			content.WriteString(page.Content)
			if page.NextOffset == nil {
				break
			}
			offset = *page.NextOffset
		}
		var input collaboration.Input
		if err := json.Unmarshal([]byte(content.String()), &input); err != nil {
			t.Fatal(err)
		}
		if input.ID != state.Inputs[index].ID || input.Body != state.Inputs[index].Body || input.Source != state.Inputs[index].Source {
			t.Fatal("accepted record changed")
		}
	}
	if _, err := databases[0].GetCollaborationRecord(ctx, fixture.workflowID, lease.AgentParticipantID, lease.ID, "human_input", 20, 0); err == nil {
		t.Fatal("read beyond frozen batch")
	}
	if _, err := databases[0].GetCollaborationRecord(ctx, fixture.workflowID, "00000000-0000-4000-8000-000000000000", lease.ID, "human_input", 0, 0); err == nil {
		t.Fatal("cross-participant read")
	}
}

func TestRetrievalOnlyInputFencesHandoffUntilEveryChunkIsRecorded(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for n, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		t.Run(string(role), func(t *testing.T) {
			database := databases[0]
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 9930+n, role, "head")
			var state collaboration.State
			for _, id := range []string{"A1", "A2"} {
				receipt := state.Accept(id, collaboration.Command{Kind: "steer", Body: strings.Repeat("\\\"界", 12000)}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: lease.AgentParticipantID, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: id}, At: time.Now()})
				if receipt.ResponseID == "" {
					t.Fatal(receipt)
				}
			}
			state.Prepare(lease.AgentParticipantID, lease.ID)
			config, _ := json.Marshal(state.Project(lease.AgentParticipantID, lease.ID))
			raw, _ := json.Marshal(state)
			if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE agent_turns SET turn_configuration=$2 WHERE id=$1`, lease.ID, config); err != nil {
				t.Fatal(err)
			}
			assertFenced := func() {
				t.Helper()
				for _, tool := range []string{"request_review", "submit_review", "confirm_prior_terminal_intent"} {
					_, err := database.ReserveMutation(ctx, lease, store.MutationSpec{ToolName: tool, OperationID: "handoff-" + tool, Request: json.RawMessage(`{"operation_id":"handoff"}`), ExternalService: "fixture", ExternalResourceID: "resource"})
					if !errors.Is(err, store.ErrPendingHumanInput) {
						t.Fatalf("%s bypassed undelivered input: %v", tool, err)
					}
				}
			}
			assertFenced()
			read := func(index, offset int, mode string) collaboration.RecordPage {
				t.Helper()
				page, err := database.GetCollaborationRecord(ctx, fixture.workflowID, lease.AgentParticipantID, lease.ID, "human_input", index, offset)
				if err != nil {
					t.Fatal(err)
				}
				args, _ := json.Marshal(map[string]any{"collection": "human_input", "index": index, "offset": offset})
				result, _ := json.Marshal(page)
				invocation := store.ReadInvocation{ToolName: "get_collaboration_record", Request: args, Result: result, StartedAt: time.Now(), FinishedAt: time.Now()}
				if mode == "failed" {
					invocation.Result = nil
					invocation.LastError = "read failed"
				}
				if mode == "forged" {
					fake := page
					fake.Content += "wrong"
					invocation.Result, _ = json.Marshal(fake)
				}
				if mode != "unrecorded" {
					_, err = databases[1].RecordReadInvocation(ctx, lease, invocation)
					if mode == "forged" {
						if err == nil {
							t.Fatal("forged content acknowledged")
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
				return page
			}
			first := read(0, 0, "unrecorded")
			assertFenced()
			if first.NextOffset == nil {
				t.Fatal("fixture must span several chunks")
			}
			read(0, 0, "failed")
			assertFenced()
			read(0, 0, "forged")
			assertFenced()
			// Last-chunk-first, duplicate reads, and a hole in the middle must not
			// acknowledge a record, even after every other record is delivered.
			var length int
			if err := pool.QueryRow(ctx, `SELECT char_length(((state->'inputs'->0)-'delivery_pending')::text) FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&length); err != nil {
				t.Fatal(err)
			}
			read(0, ((length-1)/collaboration.RecordChunkRunes)*collaboration.RecordChunkRunes, "")
			assertFenced()
			read(0, 0, "")
			read(0, 0, "")
			assertFenced()
			for index := 0; index < 2; index++ {
				offset := 0
				for {
					var page collaboration.RecordPage
					if index == 0 && offset == collaboration.RecordChunkRunes {
						page = read(index, offset, "unrecorded")
					} else {
						page = read(index, offset, "")
					}
					if page.NextOffset == nil {
						break
					}
					offset = *page.NextOffset
				}
			}
			assertFenced()
			read(0, collaboration.RecordChunkRunes, "failed")
			assertFenced()
			lastGap := read(0, collaboration.RecordChunkRunes, "")
			retried := read(0, collaboration.RecordChunkRunes, "")
			if retried.Content != lastGap.Content || retried.RecordSHA256 != lastGap.RecordSHA256 {
				t.Fatal("delivery bookkeeping changed retry contents")
			}
			pending, err := database.HasPendingHumanInput(ctx, fixture.workflowID, lease.AgentParticipantID)
			if err != nil || pending {
				t.Fatalf("complete durable coverage did not clear fence: %t %v", pending, err)
			}
			tool := "request_review"
			if role == workflow.RoleReviewer {
				tool = "submit_review"
			}
			if _, err = database.ReserveMutation(ctx, lease, store.MutationSpec{ToolName: tool, OperationID: "complete", Request: json.RawMessage(`{"operation_id":"complete"}`), ExternalService: "fixture", ExternalResourceID: "resource"}); err != nil {
				t.Fatalf("fully delivered input blocked handoff: %v", err)
			}
		})
	}
}

func TestRetrievalOnlyInputAlsoFencesSettlementAndReachesRetainedSession(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, lease, proposal := prepareSettlementTurn(t, database, pool, ctx, 9932, workflow.RoleDeveloper, "")
	var state collaboration.State
	state.Accept("A1", collaboration.Command{Kind: "steer", Body: strings.Repeat("large direction ", 8000)}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: lease.AgentParticipantID, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "input"}, At: time.Now()})
	state.Prepare(lease.AgentParticipantID, lease.ID)
	config, _ := json.Marshal(state.Project(lease.AgentParticipantID, lease.ID))
	raw, _ := json.Marshal(state)
	if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_turns SET turn_configuration=$2 WHERE id=$1`, lease.ID, config); err != nil {
		t.Fatal(err)
	}
	settled, err := database.SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomeChangeProposalReady, proposal))
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != workflow.StateDeveloping || settled.SuccessorJobID == "" {
		t.Fatal("undelivered contents bypassed settlement fence", settled)
	}
	job := claimSettlementSuccessorJob(t, database, pool, ctx, settled.SuccessorJobID, "retry-delivery")
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
	if prepared.Participant.ID != lease.AgentParticipantID || prepared.Session.ID != lease.AgentSessionID || prepared.Turn.WorkflowAttemptID != fixture.attemptID {
		t.Fatal("undelivered input lost Session/Attempt continuity")
	}
	var configObject struct {
		Projection collaboration.Projection `json:"collaboration_projection"`
	}
	if err = json.Unmarshal(prepared.Turn.TurnConfiguration, &configObject); err != nil {
		t.Fatal(err)
	}
	if !configObject.Projection.RetrievalRequired || configObject.Projection.Counts["human_input"] != 1 {
		t.Fatal("unread input not carried forward", configObject)
	}
	if err = pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(raw, &state)
	if !state.Pending(lease.AgentParticipantID) || state.Inputs[0].DeliveredTo != prepared.Turn.ID {
		t.Fatal("re-preparation dropped unread contents")
	}
}

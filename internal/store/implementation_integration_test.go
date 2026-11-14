//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/collaboration"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/github/webhook"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"strings"
	"sync"
	"testing"
	"time"
)

func seedAuthorizedHandoff(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, n int) (agentFixture, store.AgentTurnSettlement) {
	t.Helper()
	fixture, lease, _ := prepareSettlementTurn(t, database, pool, ctx, n, workflow.RolePlanner, "")
	state := collaboration.State{}
	if err := state.StartPlanning(collaboration.OriginalRequest{Title: "Rough idea", Body: "Original immutable request", Source: collaboration.Source{Channel: "fixture", ID: "original"}}); err != nil {
		t.Fatal(err)
	}
	state.Prepare(fixture.assignmentID, "proposal-turn")
	c := collaboration.PlanningContent{Title: "Accepted capability", Summary: "Behavior remains immutable", Status: "proposed", Changes: "Initial complete pair", RequirementsBrief: "Accepted behavior. No retained personal data.", TechnicalPlan: "Material commitment: local state. Naming is an adjustable suggestion.", InvestigatedCommit: strings.Repeat("a", 40), Evidence: []string{"README.md"}}
	if _, err := state.ReserveCheckpoint("pair", fixture.assignmentID, "proposal-turn", c); err != nil {
		t.Fatal(err)
	}
	if err := state.ConfirmCheckpoint("pair", collaboration.Source{Channel: "fixture", ID: "published"}); err != nil {
		t.Fatal(err)
	}
	r := state.Accept("authorize", collaboration.Command{Kind: "implement", Target: "P1"}, collaboration.Admission{Authorized: true, Active: true, ParticipantID: fixture.assignmentID, Actor: collaboration.Actor{ID: "42", Name: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "authorize"}, At: time.Now()})
	if r.ResponseID == "" {
		t.Fatal(r)
	}
	state.Prepare(fixture.assignmentID, lease.ID)
	if err := state.Assess("assessment", fixture.assignmentID, lease.ID, []collaboration.Assessment{{InputID: "authorize", EvidenceIDs: []string{"authorize"}, Disposition: "confirmed", Explanation: "Exact complete pair accepted."}}); err != nil {
		t.Fatal(err)
	}
	if err := state.ValidatePlanning("validation", "pair", fixture.assignmentID, lease.ID, strings.Repeat("a", 40), "The current repository supports every accepted commitment.", true); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(state)
	if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failures=1 WHERE id=$1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	result, err := database.SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomePlanningReady, nil))
	if err != nil || result.State != workflow.StateDeveloping || result.SuccessorJobID == "" {
		t.Fatal("handoff did not queue exactly one Developer", result, err)
	}
	return fixture, result
}
func TestImplementationAdmissionSerializesWithdrawalSteeringAndRestart(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for i, command := range []string{"withdraw-implementation", "steer", "withdraw-implementation", "steer"} {
		t.Run(fmt.Sprintf("%s-%d", command, i), func(t *testing.T) {
			fixture, settled := seedAuthorizedHandoff(t, databases[0], pool, ctx, 2300+i)
			job := claimSettlementSuccessorJob(t, databases[1], pool, ctx, settled.SuccessorJobID, "restart-preparer")
			spec := preparationSpec(strings.Repeat("a", 40), "openai/test")
			spec.VerifiedImplementationAuthorization = "authorize"
			submission := store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: command, Target: "P1"}, Actor: collaboration.Actor{ID: "42", Name: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "concurrent-human"}, Authorized: true, At: time.Now()}
			if command == "steer" {
				submission.Command.Target = ""
				submission.Command.Body = "Reconsider storage."
			}
			var prepared store.AgentTurnPreparationCommit
			var prepareErr, inputErr error
			var receipt collaboration.Receipt
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); prepared, prepareErr = databases[1].PrepareAgentTurn(ctx, *job, spec) }()
			go func() { defer wg.Done(); receipt, inputErr = databases[0].AdmitHumanInput(ctx, submission) }()
			wg.Wait()
			if inputErr != nil {
				t.Fatal(inputErr)
			}
			var raw []byte
			var turns, failures int
			var state string
			if err := pool.QueryRow(ctx, `SELECT c.state,w.status,a.infrastructure_failures,(SELECT count(*) FROM agent_turns t JOIN agent_sessions s ON s.id=t.agent_session_id JOIN agent_participants p ON p.id=s.agent_participant_id WHERE t.workflow_id=w.id AND p.role='DEVELOPER') FROM workflows w JOIN work_item_collaboration c ON c.workflow_id=w.id JOIN workflow_attempts a ON a.id=$2 WHERE w.id=$1`, fixture.workflowID, fixture.attemptID).Scan(&raw, &state, &failures, &turns); err != nil {
				t.Fatal(err)
			}
			var collaborationState collaboration.State
			if err := json.Unmarshal(raw, &collaborationState); err != nil {
				t.Fatal(err)
			}
			if prepareErr == nil {
				if turns != 1 || failures != 0 || collaborationState.Planning.Implementation.FirstDeveloperTurnID != prepared.Turn.ID {
					t.Fatal("admission not atomic", turns, failures, collaborationState)
				}
				if command == "withdraw-implementation" && (receipt.ResponseID != "" || !strings.Contains(receipt.Feedback, "close the original")) {
					t.Fatal("late withdrawal claimed stop", receipt)
				}
				var configuration map[string]json.RawMessage
				if err := json.Unmarshal(prepared.Turn.TurnConfiguration, &configuration); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(configuration["accepted_plan"]), "No retained personal data") || !strings.Contains(string(configuration["implementation_handoff"]), "Material contradictions") {
					t.Fatal("lossy accepted handoff", configuration)
				}
			} else {
				if !errors.Is(prepareErr, store.ErrAgentTurnPreparationFenceLost) || turns != 0 || failures != 1 || receipt.ResponseID == "" {
					t.Fatal("pre-admission input failed to fence", prepareErr, turns, failures, receipt)
				}
				expected := "PLANNING"
				if command == "withdraw-implementation" {
					expected = "PLAN_READY"
				}
				if state != expected {
					t.Fatal(state, expected)
				}
			}
			// Restart/replay cannot allocate a second first Developer or restore withdrawal.
			if _, err := databases[0].PrepareAgentTurn(ctx, *job, spec); err == nil {
				t.Fatal("replayed preparation allocated another Turn")
			}
			if _, err := databases[1].AdmitHumanInput(ctx, submission); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestImplementationRepositoryMovementReopensValidationWithoutRevokingGrant(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, settled := seedAuthorizedHandoff(t, databases[0], pool, ctx, 2320)
	job := claimSettlementSuccessorJob(t, databases[0], pool, ctx, settled.SuccessorJobID, "new-repository")
	spec := preparationSpec(strings.Repeat("b", 40), "openai/test")
	spec.VerifiedImplementationAuthorization = "authorize"
	if _, err := databases[1].PrepareAgentTurn(ctx, *job, spec); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
		t.Fatal(err)
	}
	var raw []byte
	var state string
	var developers, plannerJobs int
	if err := pool.QueryRow(ctx, `SELECT c.state,w.status,(SELECT count(*) FROM agent_participants WHERE workflow_id=w.id AND role='DEVELOPER'),(SELECT count(*) FROM jobs WHERE workflow_id=w.id AND kind='PREPARE_AGENT_TURN' AND status='AVAILABLE' AND payload->>'role'='PLANNER') FROM workflows w JOIN work_item_collaboration c ON c.workflow_id=w.id WHERE w.id=$1`, fixture.workflowID).Scan(&raw, &state, &developers, &plannerJobs); err != nil {
		t.Fatal(err)
	}
	var c collaboration.State
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if state != "PLANNING" || developers != 0 || plannerJobs != 1 || c.ImplementationGrant() == nil || len(c.Planning.Revisions) != 1 {
		t.Fatal("SHA movement revoked authority or admitted development", state, developers, plannerJobs, c)
	}
}

func TestImplementationClosureAndFirstAdmissionHaveOneStartBoundary(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i := 0; i < 3; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			n := 2340 + i
			fixture, settled := seedAuthorizedHandoff(t, databases[0], pool, ctx, n)
			job := claimSettlementSuccessorJob(t, databases[1], pool, ctx, settled.SuccessorJobID, "closure-race-preparer")
			spec := preparationSpec(strings.Repeat("a", 40), "openai/test")
			spec.VerifiedImplementationAuthorization = "authorize"
			delivery := workflowDelivery(fmt.Sprintf("23400000-0000-4000-8000-%012d", i+1))
			delivery.RepositoryID, delivery.IssueID, delivery.IssueNumber = int64(n), int64(n), int64(n)
			delivery.RepositoryOwner, delivery.RepositoryName = "owner", "repo"
			claim := claimWorkflowDelivery(t, databases[0], ctx, delivery)
			var prepared store.AgentTurnPreparationCommit
			var prepareErr, closeErr error
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); prepared, prepareErr = databases[1].PrepareAgentTurn(ctx, *job, spec) }()
			go func() {
				defer wg.Done()
				_, closeErr = databases[0].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, normalizedPayload(claim.DeliveryID, "closed"), store.WorkflowLocator{RepositoryID: int64(n), IssueID: int64(n), IssueNumber: int64(n)}, func(c store.WorkflowEventContext) (workflow.Event, error) {
					return workflow.IssueClosedEvent{EventMetadata: c.Metadata, ClosureID: fmt.Sprintf("23400000-0000-4000-8000-%012d", i+10), RetentionToken: fmt.Sprintf("23400000-0000-4000-8000-%012d", i+20), RetainUntil: c.Metadata.ObservedAt.Add(time.Hour)}, nil
				})
			}()
			wg.Wait()
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			var state string
			var start *string
			if err := pool.QueryRow(ctx, `SELECT w.status,c.state->'planning'->'implementation'->>'first_developer_turn_id' FROM workflows w JOIN work_item_collaboration c ON c.workflow_id=w.id WHERE w.id=$1`, fixture.workflowID).Scan(&state, &start); err != nil {
				t.Fatal(err)
			}
			if state != "CLOSING" {
				t.Fatal(state)
			}
			if prepareErr == nil {
				if start == nil || *start != prepared.Turn.ID {
					t.Fatal("admitted start not preserved")
				}
			} else if !errors.Is(prepareErr, store.ErrAgentTurnPreparationFenceLost) || start != nil {
				t.Fatal("closure did not fence admission", prepareErr, start)
			}
		})
	}
}

func TestImplementationAcceptedPairRequiresCompleteLosslessDelivery(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, settled := seedAuthorizedHandoff(t, databases[0], pool, ctx, 2360)
	// A complete original source can legitimately make the accepted record exceed
	// the inline projection budget even while the published brief/plan is concise.
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var c collaboration.State
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c.Planning.OriginalRequest.Body = strings.Repeat("original source 界\n", 4000)
	c.Planning.Revisions[0].OriginalRequest = c.Planning.OriginalRequest
	raw, _ = json.Marshal(c)
	if _, err := pool.Exec(ctx, `UPDATE work_item_collaboration SET state=$2 WHERE workflow_id=$1`, fixture.workflowID, raw); err != nil {
		t.Fatal(err)
	}
	job := claimSettlementSuccessorJob(t, databases[0], pool, ctx, settled.SuccessorJobID, "bounded-handoff")
	spec := preparationSpec(strings.Repeat("a", 40), "openai/test")
	spec.VerifiedImplementationAuthorization = "authorize"
	prepared, err := databases[0].PrepareAgentTurn(ctx, *job, spec)
	if err != nil {
		t.Fatal(err)
	}
	execution := agentTurnExecutionJob(t, pool, ctx, prepared.Turn)
	lease, err := acquireFixtureAgentTurn(t, databases[0], pool, ctx, execution, prepared.Turn.ControlRevision, "accepted-reader", time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	bindPreparedRuntime(t, databases[0], ctx, lease)
	if err = databases[0].OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	terminal := store.MutationSpec{OperationID: "handoff", ToolName: "request_review", Request: json.RawMessage(`{"operation_id":"handoff"}`), ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID}
	blocked := func() {
		t.Helper()
		if _, err := databases[1].ReserveMutation(ctx, lease, terminal); !errors.Is(err, store.ErrUnresolvedClarification) {
			t.Fatal("incomplete accepted-plan read admitted terminal handoff", err)
		}
	}
	blocked()
	offset := 0
	pages := 0
	for {
		page, err := databases[1].GetCollaborationRecord(ctx, fixture.workflowID, prepared.Participant.ID, prepared.Turn.ID, "accepted_plan", 0, offset)
		if err != nil {
			t.Fatal(err)
		}
		request, _ := json.Marshal(map[string]any{"collection": "accepted_plan", "index": 0, "offset": offset})
		result, _ := json.Marshal(page)
		if _, err = databases[0].RecordReadInvocation(ctx, lease, store.ReadInvocation{ToolName: "get_collaboration_record", Request: request, Result: result, StartedAt: time.Now(), FinishedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		pages++
		if page.NextOffset == nil {
			break
		}
		blocked()
		offset = *page.NextOffset
	}
	if pages < 2 {
		t.Fatal("fixture failed to require bounded retrieval")
	}
	if _, err = databases[1].ReserveMutation(ctx, lease, terminal); err != nil {
		t.Fatal("complete accepted record remained fenced", err)
	}
}

type implementationStatusAPI struct {
	comments []githubapi.IssueComment
	posts    int
	lost     bool
}

func (a *implementationStatusAPI) CanDecideWorkItem(context.Context, string, string, string, string, int64) (bool, error) {
	return true, nil
}
func (a *implementationStatusAPI) RepositoryCredential(context.Context, string, string) (string, error) {
	return "fixture-token", nil
}
func (a *implementationStatusAPI) ListIssueComments(context.Context, string, string, string, int) ([]githubapi.IssueComment, error) {
	return a.comments, nil
}
func (a *implementationStatusAPI) CreateIssueComment(_ context.Context, credential, _, _ string, _ int, r githubapi.CommentRequest) (githubapi.IssueComment, error) {
	a.posts++
	comment := githubapi.IssueComment{ID: int64(a.posts), Body: githubapi.JoinBodyParts(r.Body, r.Marker)}
	a.comments = append(a.comments, comment)
	if a.lost {
		a.lost = false
		return githubapi.IssueComment{}, context.DeadlineExceeded
	}
	return comment, nil
}
func TestImplementationMilestoneOutboxReconcilesLostReceiptWithoutRetryNoise(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, settled := seedAuthorizedHandoff(t, databases[0], pool, ctx, 2370)
	api := &implementationStatusAPI{lost: true}
	worker := &webhook.CollaborationStatusWorker{Store: databases[0], Adapter: &webhook.IssueCollaboration{Store: databases[0], API: api, Credentials: api}, Owner: "first-status-worker"}
	if processed, err := worker.ProcessNext(ctx); !processed || err == nil {
		t.Fatal(processed, err)
	}
	if api.posts != 1 || !strings.Contains(api.comments[0].Body, "queued for admission") || !strings.Contains(api.comments[0].Body, "P1") {
		t.Fatal(api.comments)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET available_at=clock_timestamp() WHERE workflow_id=$1 AND kind=$2`, fixture.workflowID, store.PublishCollaborationStatusJobKind); err != nil {
		t.Fatal(err)
	}
	// A new Store/worker observes the reserved exact artifact after restart.
	worker.Store = databases[1]
	worker.Adapter.Store = databases[1]
	if processed, err := worker.ProcessNext(ctx); !processed || err != nil {
		t.Fatal(processed, err)
	}
	if api.posts != 1 {
		t.Fatal("retry duplicated milestone", api.posts)
	}
	job := claimSettlementSuccessorJob(t, databases[0], pool, ctx, settled.SuccessorJobID, "admission-after-status")
	spec := preparationSpec(strings.Repeat("a", 40), "openai/test")
	spec.VerifiedImplementationAuthorization = "authorize"
	if _, err := databases[0].PrepareAgentTurn(ctx, *job, spec); err != nil {
		t.Fatal(err)
	}
	if processed, err := worker.ProcessNext(ctx); !processed || err != nil {
		t.Fatal(processed, err)
	}
	if api.posts != 2 || !strings.Contains(api.comments[1].Body, "first Developer Agent Turn is durably admitted") {
		t.Fatal(api.comments)
	}
	if processed, err := worker.ProcessNext(ctx); processed || err != nil {
		t.Fatal("per-retry status noise", processed, err)
	}
}

func TestImplementationWithdrawalBeforeAdmissionRequiresFreshConsentButNotNewDocuments(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, settled := seedAuthorizedHandoff(t, databases[0], pool, ctx, 2380)
	job := claimSettlementSuccessorJob(t, databases[0], pool, ctx, settled.SuccessorJobID, "unused-preparation")
	submission := store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "withdraw-implementation", Target: "P1"}, Actor: collaboration.Actor{ID: "42", Name: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "withdraw-unused"}, Authorized: true, At: time.Now()}
	receipt, err := databases[0].AdmitHumanInput(ctx, submission)
	if err != nil || receipt.ResponseID == "" {
		t.Fatal(receipt, err)
	}
	spec := preparationSpec(strings.Repeat("a", 40), "openai/test")
	spec.VerifiedImplementationAuthorization = "authorize"
	if _, err = databases[1].PrepareAgentTurn(ctx, *job, spec); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
		t.Fatal("withdrawn preparation admitted", err)
	}
	submission.Command.Kind = "implement"
	submission.Source.ID = "authorize"
	if _, err = databases[1].AdmitHumanInput(ctx, submission); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	var status string
	var jobs int
	inspect := func() {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT c.state,w.status,(SELECT count(*) FROM jobs WHERE workflow_id=w.id AND kind='PREPARE_AGENT_TURN' AND status IN ('AVAILABLE','LEASED')) FROM workflows w JOIN work_item_collaboration c ON c.workflow_id=w.id WHERE w.id=$1`, fixture.workflowID).Scan(&raw, &status, &jobs); err != nil {
			t.Fatal(err)
		}
	}
	inspect()
	var state collaboration.State
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if status != "PLAN_READY" || jobs != 0 || state.ImplementationGrant() != nil || state.Planning.ReadyRevision != "pair" {
		t.Fatal("replay undid withdrawal or pair confirmation", status, jobs, state)
	}
	submission.Source.ID = "fresh-authorization"
	submission.At = time.Now()
	receipt, err = databases[1].AdmitHumanInput(ctx, submission)
	if err != nil || receipt.ResponseID == "" {
		t.Fatal(receipt, err)
	}
	inspect()
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if status != "PLANNING" || jobs != 1 || state.ImplementationGrant() == nil || len(state.Planning.Revisions) != 1 {
		t.Fatal("fresh consent republished documents or bypassed Planner", status, jobs, state)
	}
}

func TestImplementationUpgradeExposesReadyPairWithoutGrantOrDiscussionReplay(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, settled := seedAuthorizedHandoff(t, databases[0], pool, ctx, 2390)
	// Isolated pre-feature state: same immutable published/confirmed pair, but no
	// implementation actions or outbox from this feature have ever existed.
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var c collaboration.State
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c.Planning.Implementation = collaboration.ImplementationState{}
	c.Inputs[0].Kind = "confirm-plan"
	c.Receipts = nil
	raw, _ = json.Marshal(c)
	if _, err := pool.Exec(ctx, `UPDATE work_item_collaboration SET state=$2 WHERE workflow_id=$1;`, fixture.workflowID, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status='PLAN_READY',planning_only=true WHERE id=$1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET current_stage='planning' WHERE workflow_id=$1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status='CANCELLED' WHERE workflow_id=$1 AND (kind=$2 OR id=$3)`, fixture.workflowID, store.PublishCollaborationStatusJobKind, settled.SuccessorJobID); err != nil {
		t.Fatal(err)
	}
	for _, database := range databases {
		if err := database.EnsurePlanningAuthorizationPrompts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var prompts, turns int
	if err := pool.QueryRow(ctx, `SELECT state,(SELECT count(*) FROM jobs WHERE workflow_id=$1 AND kind=$2 AND status='AVAILABLE'),(SELECT count(*) FROM agent_turns WHERE workflow_id=$1) FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID, store.PublishCollaborationStatusJobKind).Scan(&raw, &prompts, &turns); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if prompts != 1 || turns != 1 || c.ImplementationGrant() != nil || len(c.Planning.Revisions) != 1 || !strings.Contains(c.Receipts[0].Feedback, "/omnigrex implement P1") {
		t.Fatal("upgrade authorized work, repeated discussion, or hid actual action", prompts, turns, c)
	}
}

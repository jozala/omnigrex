//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func replanningState(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id string) collaboration.State {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var s collaboration.State
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}
func runPreparedReplanningTurn(t *testing.T, db *store.Store, pool *pgxpool.Pool, ctx context.Context, prepared store.AgentTurnPreparationCommit) store.AgentTurnLease {
	t.Helper()
	job := agentTurnExecutionJob(t, pool, ctx, prepared.Turn)
	lease, err := acquireFixtureAgentTurn(t, db, pool, ctx, job, prepared.Turn.ControlRevision, "replanning-runtime", 20*time.Second, 100)
	if err != nil {
		t.Fatal(err)
	}
	bindPreparedRuntime(t, db, ctx, lease)
	if err = db.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	return lease
}
func recordReplanningMutation(t *testing.T, db *store.Store, ctx context.Context, lease store.AgentTurnLease, label, tool string, args any, run func(string) any) store.MutationReservation {
	t.Helper()
	raw, _ := json.Marshal(args)
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		t.Fatal("invalid fixture arguments")
	}
	object["operation_id"], _ = json.Marshal(label)
	raw, _ = json.Marshal(object)
	m, err := db.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: label, ToolName: tool, Request: raw, ExternalService: "omnigrex", ExternalResourceID: lease.JobLease.WorkflowID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.StartMutation(ctx, lease, m.ID); err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(run(m.ID))
	if err = db.CompleteMutation(ctx, lease, m.ID, result); err != nil {
		t.Fatal(err)
	}
	return m
}

// A false alarm is charged only at settlement, resumes the original Developer
// under consumed-but-valid authority, and cannot erase existing budget usage.
func TestReplanningUnchangedSettlementAndRetainedAdmission(t *testing.T) {
	for _, handoff := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained-admission", true: "unused-withdrawal-in-handoff"}[handoff], func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 2)
			db := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			fixture, initial := seedAuthorizedHandoff(t, db, pool, ctx, 2360)
			job := claimSettlementSuccessorJob(t, db, pool, ctx, initial.SuccessorJobID, "initial-developer")
			spec := preparationSpec(strings.Repeat("a", 40), "openai/test")
			spec.VerifiedImplementationAuthorization = "authorize"
			prepared, err := db.PrepareAgentTurn(ctx, *job, spec)
			if err != nil {
				t.Fatal(err)
			}
			lease := runPreparedReplanningTurn(t, db, pool, ctx, prepared)
			first := lease.ID
			if _, err = pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failures=1,review_usage='{"review":2}' WHERE id=$1`, fixture.attemptID); err != nil {
				t.Fatal(err)
			}
			concern := collaboration.ReplanningConcern{AcceptedRevisionID: "pair", Commitment: "Local state and no retained personal data", Evidence: []string{"An isolated restart test appears to retain a user label."}, Consequences: "Potential retention conflict."}
			request := concern
			var cycle collaboration.ReplanningCycle
			recordReplanningMutation(t, db, ctx, lease, "concern", "request_replanning", request, func(id string) any {
				cycle, err = db.RequestReplanning(ctx, lease, id, concern)
				if err != nil {
					t.Fatal(err)
				}
				return map[string]any{"status": "recorded", "handoff": "pending_settlement", "outcome": "REPLANNING_REQUESTED", "cycle_id": cycle.Scope.OperationID, "accepted_revision_id": cycle.Concern.AcceptedRevisionID}
			})
			var charge int
			var status string
			if err = pool.QueryRow(ctx, `SELECT a.autonomous_replanning_returns,w.status FROM workflow_attempts a JOIN workflows w ON w.id=a.workflow_id WHERE a.id=$1`, fixture.attemptID).Scan(&charge, &status); err != nil {
				t.Fatal(err)
			}
			if charge != 0 || status != "DEVELOPING" {
				t.Fatal("request dispatched before safe settlement", charge, status)
			}
			if err = db.CloseMutationAdmission(ctx, lease); err != nil {
				t.Fatal(err)
			}
			settled, err := db.SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomeReplanningRequested, nil))
			if err != nil {
				t.Fatal(err)
			}
			if settled.State != workflow.StatePlanning || settled.SuccessorJobID == "" {
				t.Fatal(settled)
			}
			replay, err := databases[1].SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomeReplanningRequested, nil))
			if err != nil || replay != settled {
				t.Fatal(replay, err)
			}
			job = claimSettlementSuccessorJob(t, databases[1], pool, ctx, settled.SuccessorJobID, "retained-planner")
			// Reuse the exact prior Planner binding; a different profile/image would
			// exercise the separate existing compatibility recovery path.
			var commit string
			var hash, config []byte
			if err = pool.QueryRow(ctx, `SELECT agent_profile_commit_sha,agent_profile_content_sha256,agent_profile_config FROM agent_turns WHERE id=(SELECT id FROM agent_turns WHERE workflow_id=$1 AND stage_id='planning' ORDER BY turn_number LIMIT 1)`, fixture.workflowID).Scan(&commit, &hash, &config); err != nil {
				t.Fatal(err)
			}
			spec.Stages[workflow.StagePlanning] = store.ParticipantPreparation{ProfilePath: ".omnigrex/team/planner.md", Profile: store.AgentProfileSnapshot{CommitSHA: strings.Repeat("a", 40), ContentSHA256: hash, Config: config}, Binding: store.ParticipantRuntimeBinding{AgentProfileName: "planner", RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2", RuntimeProfileContentSHA256: strings.Repeat("a", 64), RuntimeImageDigest: "sha256:test"}}
			planned, err := databases[1].PrepareAgentTurn(ctx, *job, spec)
			if err != nil {
				t.Fatal(err)
			}
			if planned.Participant.ID != fixture.assignmentID || planned.Session.ID != fixture.sessionID {
				t.Fatal("replaced retained Planner", planned.Participant, planned.Session)
			}
			lease = runPreparedReplanningTurn(t, databases[1], pool, ctx, planned)
			// The current default-branch fact is a successful scoped read, not agent prose.
			if _, err = databases[1].RecordReadInvocation(ctx, lease, store.ReadInvocation{StartedAt: time.Now(), FinishedAt: time.Now(), ToolName: "refresh_default_branch", Request: json.RawMessage(`{"operation_id":"current"}`), Result: json.RawMessage(`{"status":"current","sha":"` + strings.Repeat("a", 40) + `"}`)}); err != nil {
				t.Fatal(err)
			}
			recordReplanningMutation(t, databases[1], ctx, lease, "resolve", "complete_planning", map[string]any{"operation_id": "resolve", "revision_id": "pair", "repository_commit": strings.Repeat("a", 40), "evidence": "The restart test read a fixture label, not persisted personal data.", "material_decisions_resolved": true, "disposition": "unchanged"}, func(id string) any {
				if err = databases[1].CompleteReplanning(ctx, lease, id, "pair", strings.Repeat("a", 40), "The restart test read a fixture label, not persisted personal data.", "unchanged", true); err != nil {
					t.Fatal(err)
				}
				return map[string]any{"outcome": "REPLANNING_RESOLVED", "revision_id": "pair", "repository_commit": strings.Repeat("a", 40)}
			})
			if err = databases[1].CloseMutationAdmission(ctx, lease); err != nil {
				t.Fatal(err)
			}
			resolved, err := databases[1].SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomeReplanningResolved, nil))
			if err != nil || resolved.State != workflow.StateDeveloping {
				t.Fatal(resolved, err)
			}
			job = claimSettlementSuccessorJob(t, db, pool, ctx, resolved.SuccessorJobID, "retained-developer")
			if handoff {
				if _, err = databases[0].AcknowledgeAgentTurnPreparationFailure(ctx, *job, errors.New("Developer profile unavailable"), false, 0); err != nil {
					t.Fatal(err)
				}
				inspect := func() string {
					t.Helper()
					var raw []byte
					if err = pool.QueryRow(ctx, `SELECT jsonb_build_array(w.status,w.state_revision,a.current_stage,a.infrastructure_failures,a.review_usage,a.autonomous_replanning_returns,(SELECT count(*) FROM agent_turns WHERE workflow_id=w.id),(SELECT count(*) FROM jobs WHERE workflow_id=w.id AND status IN ('AVAILABLE','LEASED'))) FROM workflows w JOIN workflow_attempts a ON a.workflow_id=w.id AND a.active WHERE w.id=$1`, fixture.workflowID).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					return string(raw)
				}
				before := inspect()
				input := store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "withdraw-implementation", Target: "P1"}, Actor: collaboration.Actor{ID: "42", Name: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "withdraw-unused-resumption"}, Authorized: true, At: time.Now()}
				r, e := databases[1].AdmitHumanInput(ctx, input)
				if e != nil || r.ResponseID == "" {
					t.Fatal(r, e)
				}
				replay, e := databases[0].AdmitHumanInput(ctx, input)
				if e != nil || replay.ResponseID != r.ResponseID {
					t.Fatal(replay, e)
				}
				state := replanningState(t, pool, ctx, fixture.workflowID)
				if before != inspect() || !strings.Contains(before, "NEEDS_HUMAN") || state.ResumptionReady() || state.Planning.Implementation.FirstDeveloperTurnID != first {
					t.Fatal("unused withdrawal resumed/reset work or retained invalid authority", before, inspect(), state)
				}
				input.Command = collaboration.Command{Kind: "implement", Target: "P1"}
				input.Source.ID = "cannot-resume-handoff"
				if r, e = databases[0].AdmitHumanInput(ctx, input); e != nil || r.ResponseID != "" {
					t.Fatal("authorization escaped Human Handoff", r, e)
				}
				return
			}
			spec.VerifiedImplementationAuthorization = ""
			resumed, err := db.PrepareAgentTurn(ctx, *job, spec)
			if err != nil {
				t.Fatal(err)
			}
			if resumed.Participant.ID != prepared.Participant.ID || resumed.Session.ID != prepared.Session.ID {
				t.Fatal("replaced Developer")
			}
			state := replanningState(t, pool, ctx, fixture.workflowID)
			if state.ActiveReplanning() != nil || state.Planning.Implementation.FirstDeveloperTurnID != first || len(state.Planning.Revisions) != 1 || len(state.Planning.Implementation.Bindings) != 2 {
				t.Fatal("lost original admission or fabricated pair", state)
			}
			var failures, usage int
			if err = pool.QueryRow(ctx, `SELECT autonomous_replanning_returns,infrastructure_failures,(review_usage->>'review')::int FROM workflow_attempts WHERE id=$1`, fixture.attemptID).Scan(&charge, &failures, &usage); err != nil {
				t.Fatal(err)
			}
			if charge != 1 || failures != 1 || usage != 2 {
				t.Fatal("resumption reset budgets", charge, failures, usage)
			}
		})
	}
}

// The fourth needed return retains the concern without a phantom dispatch.
// Explicit human recovery starts a new Attempt using existing recovery rules.
func TestReplanningFourthReturnRetainsEvidenceUntilExplicitRecovery(t *testing.T) {
	dbs, pool := openPhaseFiveStores(t, 2)
	db := dbs[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fixture, initial := seedAuthorizedHandoff(t, db, pool, ctx, 2361)
	job := claimSettlementSuccessorJob(t, db, pool, ctx, initial.SuccessorJobID, "developer")
	spec := preparationSpec(strings.Repeat("a", 40), "openai/test")
	spec.VerifiedImplementationAuthorization = "authorize"
	prepared, err := db.PrepareAgentTurn(ctx, *job, spec)
	if err != nil {
		t.Fatal(err)
	}
	lease := runPreparedReplanningTurn(t, db, pool, ctx, prepared)
	if _, err = pool.Exec(ctx, `UPDATE workflow_attempts SET autonomous_replanning_returns=3,review_usage='{"review":2}' WHERE id=$1`, fixture.attemptID); err != nil {
		t.Fatal(err)
	}
	concern := collaboration.ReplanningConcern{AcceptedRevisionID: "pair", Commitment: "Compatibility", Evidence: []string{"An isolated legacy consumer fails after the accepted design change."}, Consequences: "Existing consumers cannot continue."}
	recordReplanningMutation(t, db, ctx, lease, "fourth", "request_replanning", concern, func(id string) any {
		c, e := db.RequestReplanning(ctx, lease, id, concern)
		if e != nil {
			t.Fatal(e)
		}
		return map[string]any{"status": "recorded", "handoff": "pending_settlement", "outcome": "REPLANNING_REQUESTED", "cycle_id": c.Scope.OperationID, "accepted_revision_id": "pair"}
	})
	if err = db.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	stopped, err := db.SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomeReplanningRequested, nil))
	if err != nil || stopped.State != workflow.StateNeedsHuman || stopped.SuccessorJobID != "" {
		t.Fatal(stopped, err)
	}
	s := replanningState(t, pool, ctx, fixture.workflowID)
	if c := s.ActiveReplanning(); c == nil || c.Phase != collaboration.ReplanningRequested || c.DispatchedAttemptID != "" {
		t.Fatal("phantom fourth dispatch", s)
	}
	delivery := workflowDelivery(uuid.NewString())
	delivery.RepositoryID = 2361
	delivery.IssueID = 2361
	delivery.IssueNumber = 2361
	delivery.RepositoryOwner = "owner"
	delivery.RepositoryName = "repo"
	claim := claimWorkflowDelivery(t, dbs[1], ctx, delivery)
	application, err := dbs[1].CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, normalizedPayload(claim.DeliveryID, "trigger"), store.WorkflowLocator{RepositoryID: 2361, IssueID: 2361, IssueNumber: 2361}, func(c store.WorkflowEventContext) (workflow.Event, error) {
		return workflow.TriggerEvent{EventMetadata: c.Metadata, AttemptID: uuid.NewString(), AttemptNumber: 2}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if application.State != workflow.StatePlanning {
		t.Fatal("recovery routed around retained concern", application)
	}
	var oldCount, newCount, plannerJobs int
	if err = pool.QueryRow(ctx, `SELECT (SELECT autonomous_replanning_returns FROM workflow_attempts WHERE id=$1),(SELECT autonomous_replanning_returns FROM workflow_attempts WHERE workflow_id=$2 AND active),(SELECT count(*) FROM jobs WHERE workflow_id=$2 AND status='AVAILABLE' AND kind='PREPARE_AGENT_TURN' AND payload->>'role'='PLANNER')`, fixture.attemptID, fixture.workflowID).Scan(&oldCount, &newCount, &plannerJobs); err != nil {
		t.Fatal(err)
	}
	s = replanningState(t, pool, ctx, fixture.workflowID)
	if oldCount != 3 || newCount != 1 || plannerJobs != 1 || s.ActiveReplanning().Phase != collaboration.ReplanningActive {
		t.Fatal("recovery did not dispatch once in new Attempt", oldCount, newCount, plannerJobs, s)
	}
}

func TestReplanningLostResponseRecoversOriginalRequestOnlyAfterStop(t *testing.T) {
	dbs, pool := openPhaseFiveStores(t, 2)
	db := dbs[0]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, initial := seedAuthorizedHandoff(t, db, pool, ctx, 2362)
	job := claimSettlementSuccessorJob(t, db, pool, ctx, initial.SuccessorJobID, "developer")
	spec := preparationSpec(strings.Repeat("a", 40), "openai/test")
	spec.VerifiedImplementationAuthorization = "authorize"
	prepared, err := db.PrepareAgentTurn(ctx, *job, spec)
	if err != nil {
		t.Fatal(err)
	}
	lease := runPreparedReplanningTurn(t, db, pool, ctx, prepared)
	concern := collaboration.ReplanningConcern{AcceptedRevisionID: "pair", Commitment: "Compatibility", Evidence: []string{"An isolated legacy consumer fails."}, Consequences: "The accepted compatibility commitment is doubtful.", HumanInputIDs: []string{}}
	raw, _ := json.Marshal(concern)
	var args map[string]json.RawMessage
	_ = json.Unmarshal(raw, &args)
	args["operation_id"] = json.RawMessage(`"lost-request"`)
	args["human_input_ids"] = json.RawMessage(`[]`)
	raw, _ = json.Marshal(args)
	m, err := db.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "lost-request", ToolName: "request_replanning", Request: raw, ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.StartMutation(ctx, lease, m.ID); err != nil {
		t.Fatal(err)
	}
	mismatched := concern
	mismatched.Consequences = "A different request cannot reuse this operation identity."
	if _, e := db.RequestReplanning(ctx, lease, m.ID, mismatched); !errors.Is(e, collaboration.ErrInvalid) {
		t.Fatal("accepted mismatching reserved request", e)
	}
	rejectedState := replanningState(t, pool, ctx, fixture.workflowID)
	if rejectedState.ActiveReplanning() != nil {
		t.Fatal("rejected request recorded a cycle")
	}
	c, err := db.RequestReplanning(ctx, lease, m.ID, concern)
	if err != nil {
		t.Fatal(err)
	}
	if _, e := db.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "contradictory-publication", ToolName: "publish_changes", Request: json.RawMessage(`{}`), ExternalService: "github", ExternalResourceID: "scope"}); !errors.Is(e, store.ErrMutationOperationConflict) {
		t.Fatal("recorded terminal request admitted contradictory mutation", e)
	}
	// The effect commits, but no successful mutation response is recorded.
	if err = db.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
	if _, err = db.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	before := replanningState(t, pool, ctx, fixture.workflowID)
	if before.ActiveReplanning().Phase != collaboration.ReplanningRequested {
		t.Fatal("request dispatched during expiry")
	}
	stop := claimRecoveryJob(t, db, ctx, store.StopStaleRuntimeJobKind, "replanning-stop")
	if _, err = db.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &replayGatewayGitHub{}, Credentials: replayGatewayCredentials{}, Publications: unusedHandoffPublications{}, Clarifications: dbs[1]})
	if err != nil {
		t.Fatal(err)
	}
	mutations, err := mcp.NewRecoveryWorker(dbs[1], reconciler, mcp.RecoveryWorkerConfig{ClaimOwner: "replanning-recovery", LeaseDuration: time.Minute, HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if processed, e := mutations.ProcessNext(ctx); e != nil || !processed {
		t.Fatal(processed, e)
	}
	var mutationState, status string
	var count int
	if err = pool.QueryRow(ctx, `SELECT i.state,w.status,a.autonomous_replanning_returns FROM tool_invocations i JOIN workflows w ON w.id=$2 JOIN workflow_attempts a ON a.id=$3 WHERE i.id=$1`, m.ID, fixture.workflowID, fixture.attemptID).Scan(&mutationState, &status, &count); err != nil {
		t.Fatal(err)
	}
	if mutationState != "SUCCEEDED" || status != "DEVELOPING" || count != 0 {
		t.Fatal("reconciliation dispatched without settlement", mutationState, status, count)
	}
	workspaces, _ := integrationTerminalWorkspace(t, pool, lease, committedPublicationPaths(t), "head", "installation-token")
	credential := &integrationCredentialProvider{credential: "installation-token"}
	api := &replayOutcomeGitHub{}
	outcomes, err := agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: dbs[1], GitHub: api, Workspace: workspaces, DeveloperCredentials: credential, ReviewerCredentials: credential})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := agentturn.NewTerminalCorroborationWorker(dbs[1], outcomes, credential, credential, workspaces, agentturn.TerminalCorroborationWorkerConfig{ClaimOwner: "replanning-terminal", Window: time.Minute, PollInterval: time.Millisecond, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if processed, e := worker.ProcessOne(ctx); e != nil || !processed {
		t.Fatal(processed, e)
	}
	after := replanningState(t, pool, ctx, fixture.workflowID)
	if after.ActiveReplanning().Scope.OperationID != c.Scope.OperationID || after.ActiveReplanning().Phase != collaboration.ReplanningActive || len(after.Replanning.Cycles) != 1 {
		t.Fatal("lost response duplicated or lost concern", after)
	}
	if err = pool.QueryRow(ctx, `SELECT w.status,a.autonomous_replanning_returns FROM workflows w JOIN workflow_attempts a ON a.id=$2 WHERE w.id=$1`, fixture.workflowID, fixture.attemptID).Scan(&status, &count); err != nil {
		t.Fatal(err)
	}
	if status != "PLANNING" || count != 1 || api.calls != 0 {
		t.Fatal(status, count, api.calls)
	}
	if processed, e := worker.ProcessOne(ctx); e != nil || processed {
		t.Fatal("duplicate terminal transfer", processed, e)
	}

	// Continue the retained Planner, then lose its resolution acknowledgement as
	// well. Neither recovery boundary is allowed to dispatch or charge twice.
	var plannerJob string
	if err = pool.QueryRow(ctx, `SELECT id::text FROM jobs WHERE workflow_id=$1 AND kind='PREPARE_AGENT_TURN' AND status='AVAILABLE' AND payload->>'role'='PLANNER'`, fixture.workflowID).Scan(&plannerJob); err != nil {
		t.Fatal(err)
	}
	job = claimSettlementSuccessorJob(t, db, pool, ctx, plannerJob, "lost-resolution-planner")
	var hash, config []byte
	if err = pool.QueryRow(ctx, `SELECT agent_profile_content_sha256,agent_profile_config FROM agent_turns WHERE workflow_id=$1 AND stage_id='planning' ORDER BY turn_number LIMIT 1`, fixture.workflowID).Scan(&hash, &config); err != nil {
		t.Fatal(err)
	}
	spec.Stages[workflow.StagePlanning] = store.ParticipantPreparation{ProfilePath: ".omnigrex/team/planner.md", Profile: store.AgentProfileSnapshot{CommitSHA: strings.Repeat("a", 40), ContentSHA256: hash, Config: config}, Binding: store.ParticipantRuntimeBinding{AgentProfileName: "planner", RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2", RuntimeProfileContentSHA256: strings.Repeat("a", 64), RuntimeImageDigest: "sha256:test"}}
	planned, err := db.PrepareAgentTurn(ctx, *job, spec)
	if err != nil {
		t.Fatal(err)
	}
	lease = runPreparedReplanningTurn(t, db, pool, ctx, planned)
	if _, err = db.RecordReadInvocation(ctx, lease, store.ReadInvocation{StartedAt: time.Now(), FinishedAt: time.Now(), ToolName: "refresh_default_branch", Request: json.RawMessage(`{"operation_id":"current"}`), Result: json.RawMessage(`{"status":"current","sha":"` + strings.Repeat("a", 40) + `"}`)}); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]any{"operation_id": "lost-resolution", "revision_id": "pair", "repository_commit": strings.Repeat("a", 40), "evidence": "The isolated legacy consumer used an unsupported fixture configuration; commitments remain valid.", "material_decisions_resolved": true, "disposition": "unchanged"})
	m, err = db.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: "lost-resolution", ToolName: "complete_planning", Request: raw, ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.StartMutation(ctx, lease, m.ID); err != nil {
		t.Fatal(err)
	}
	if err = db.CompleteReplanning(ctx, lease, m.ID, "pair", strings.Repeat("a", 40), "The isolated legacy consumer used an unsupported fixture configuration; commitments remain valid.", "unchanged", true); err != nil {
		t.Fatal(err)
	}
	if err = db.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
	if _, err = db.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	stop = claimRecoveryJob(t, db, ctx, store.StopStaleRuntimeJobKind, "replanning-resolution-stop")
	if _, err = db.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	if processed, e := mutations.ProcessNext(ctx); e != nil || !processed {
		t.Fatal(processed, e)
	}
	if err = pool.QueryRow(ctx, `SELECT i.state,w.status,a.autonomous_replanning_returns FROM tool_invocations i JOIN workflows w ON w.id=$2 JOIN workflow_attempts a ON a.id=$3 WHERE i.id=$1`, m.ID, fixture.workflowID, fixture.attemptID).Scan(&mutationState, &status, &count); err != nil {
		t.Fatal(err)
	}
	if mutationState != "SUCCEEDED" || status != "PLANNING" || count != 1 {
		t.Fatal("resolution recovery resumed without settlement", mutationState, status, count)
	}
	workspaces, _ = integrationTerminalWorkspace(t, pool, lease, committedPublicationPaths(t), "head", "installation-token")
	outcomes, err = agentturn.NewOutcomeReconciler(agentturn.OutcomeReconcilerConfig{Store: dbs[1], GitHub: api, Workspace: workspaces, DeveloperCredentials: credential, ReviewerCredentials: credential})
	if err != nil {
		t.Fatal(err)
	}
	worker, err = agentturn.NewTerminalCorroborationWorker(dbs[1], outcomes, credential, credential, workspaces, agentturn.TerminalCorroborationWorkerConfig{ClaimOwner: "replanning-resolution-terminal", Window: time.Minute, PollInterval: time.Millisecond, LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if processed, e := worker.ProcessOne(ctx); e != nil || !processed {
		t.Fatal(processed, e)
	}
	if err = pool.QueryRow(ctx, `SELECT w.status,a.autonomous_replanning_returns FROM workflows w JOIN workflow_attempts a ON a.id=$2 WHERE w.id=$1`, fixture.workflowID, fixture.attemptID).Scan(&status, &count); err != nil {
		t.Fatal(err)
	}
	after = replanningState(t, pool, ctx, fixture.workflowID)
	if status != "DEVELOPING" || count != 1 || api.calls != 0 || after.ActiveReplanning() == nil || after.ActiveReplanning().Phase != collaboration.ReplanningQueued || after.Planning.Implementation.FirstDeveloperTurnID != prepared.Turn.ID || len(after.Replanning.Cycles) != 1 {
		t.Fatal("lost resolution duplicated authority or lost continuity", status, count, api.calls, after)
	}
	if processed, e := worker.ProcessOne(ctx); e != nil || processed {
		t.Fatal("duplicate resumed dispatch", processed, e)
	}
}

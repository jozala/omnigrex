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
	"github.com/jozala/omnigrex/internal/collaboration"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func reviewReplanningDeveloper(t *testing.T, db *store.Store, pool *pgxpool.Pool, ctx context.Context, n int) (agentFixture, store.AgentTurnLease, store.AgentTurnPreparationSpec) {
	t.Helper()
	fixture, initial := seedAuthorizedHandoff(t, db, pool, ctx, n)
	spec := preparationSpec(strings.Repeat("a", 40), "openai/test")
	spec.VerifiedImplementationAuthorization = "authorize"
	job := claimSettlementSuccessorJob(t, db, pool, ctx, initial.SuccessorJobID, "review-developer")
	prepared, err := db.PrepareAgentTurn(ctx, *job, spec)
	if err != nil {
		t.Fatal(err)
	}
	return fixture, runPreparedReplanningTurn(t, db, pool, ctx, prepared), spec
}
func reviewReplanningContinue(t *testing.T, db *store.Store, pool *pgxpool.Pool, ctx context.Context, lease store.AgentTurnLease, spec store.AgentTurnPreparationSpec, outcome workflow.TurnOutcome) store.AgentTurnLease {
	t.Helper()
	if err := db.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	settled, err := db.SettleAgentTurn(ctx, lease, successfulSettlementObservation(outcome, nil))
	if err != nil || settled.SuccessorJobID == "" {
		t.Fatal(settled, err)
	}
	job := claimSettlementSuccessorJob(t, db, pool, ctx, settled.SuccessorJobID, "review-continuation")
	prepared, err := db.PrepareAgentTurn(ctx, *job, spec)
	if err != nil {
		t.Fatal(err)
	}
	return runPreparedReplanningTurn(t, db, pool, ctx, prepared)
}
func reviewReplanningInput(t *testing.T, db *store.Store, ctx context.Context, workflowID string, command collaboration.Command) string {
	t.Helper()
	receipt, err := db.AdmitHumanInput(ctx, store.HumanInputSubmission{WorkflowID: workflowID, Command: command, Actor: collaboration.Actor{ID: "42", Name: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: uuid.NewString()}, Authorized: true, At: time.Now()})
	if err != nil || receipt.ResponseID == "" {
		t.Fatal(receipt, err)
	}
	return receipt.ResponseID
}
func reviewReplanningAssessment(t *testing.T, db *store.Store, ctx context.Context, lease store.AgentTurnLease, id, disposition string) {
	t.Helper()
	batch := []collaboration.Assessment{{InputID: id, EvidenceIDs: []string{id}, Disposition: disposition, Explanation: "Explicit interpretation of the delivered immutable evidence."}}
	recordReplanningMutation(t, db, ctx, lease, "assess-"+uuid.NewString(), "assess_human_input", map[string]any{"assessments": batch}, func(op string) any {
		if err := db.AssessHumanInput(ctx, lease, op, batch); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"recorded": true}
	})
}
func reviewReplanningConcern(t *testing.T, db *store.Store, pool *pgxpool.Pool, ctx context.Context, fixture agentFixture, lease store.AgentTurnLease, spec store.AgentTurnPreparationSpec, humanInputIDs ...string) (store.AgentTurnLease, store.AgentTurnPreparationSpec) {
	t.Helper()
	concern := collaboration.ReplanningConcern{AcceptedRevisionID: "pair", Commitment: "No retained personal data", Evidence: []string{"The restart experiment makes the accepted retention commitment doubtful."}, Consequences: "A product decision may be needed.", HumanInputIDs: humanInputIDs}
	recordReplanningMutation(t, db, ctx, lease, "concern", "request_replanning", concern, func(op string) any {
		c, err := db.RequestReplanning(ctx, lease, op, concern)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]any{"status": "recorded", "handoff": "pending_settlement", "outcome": "REPLANNING_REQUESTED", "cycle_id": c.Scope.OperationID, "accepted_revision_id": "pair"}
	})
	var hash, config []byte
	if err := pool.QueryRow(ctx, `SELECT agent_profile_content_sha256,agent_profile_config FROM agent_turns WHERE workflow_id=$1 AND stage_id='planning' ORDER BY turn_number LIMIT 1`, fixture.workflowID).Scan(&hash, &config); err != nil {
		t.Fatal(err)
	}
	spec.Stages[workflow.StagePlanning] = store.ParticipantPreparation{ProfilePath: ".omnigrex/team/planner.md", Profile: store.AgentProfileSnapshot{CommitSHA: strings.Repeat("a", 40), ContentSHA256: hash, Config: config}, Binding: store.ParticipantRuntimeBinding{AgentProfileName: "planner", RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2", RuntimeProfileContentSHA256: strings.Repeat("a", 64), RuntimeImageDigest: "sha256:test"}}
	return reviewReplanningContinue(t, db, pool, ctx, lease, spec, workflow.TurnOutcomeReplanningRequested), spec
}
func reviewReplanningQuestion(t *testing.T, db *store.Store, ctx context.Context, lease store.AgentTurnLease) collaboration.Round {
	t.Helper()
	var round collaboration.Round
	questions := []collaboration.Question{{ID: "retention", Body: "Which retained state is allowed?"}}
	recordReplanningMutation(t, db, ctx, lease, "question", "request_clarification", map[string]any{"questions": questions}, func(op string) any {
		var err error
		round, err = db.RequestClarification(ctx, lease, op, questions)
		if err != nil {
			t.Fatal(err)
		}
		if err = db.ConfirmClarificationPublication(ctx, lease.JobLease.WorkflowID, round.ID, collaboration.Source{Channel: "fixture", ID: "question"}); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"round_id": round.ID}
	})
	return round
}

func TestReplanningReviewUnresolvedEvidenceCanTransferButNotResume(t *testing.T) {
	for i, kind := range []string{"uncertain-steering", "unresolved-question", "uncertain-steering-referenced"} {
		t.Run(kind, func(t *testing.T) {
			dbs, pool := openPhaseFiveStores(t, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			db := dbs[0]
			fixture, lease, spec := reviewReplanningDeveloper(t, db, pool, ctx, 2460+i)
			command := collaboration.Command{Kind: "steer", Body: "The retention commitment may conflict with required restart behavior."}
			if kind == "unresolved-question" {
				round := reviewReplanningQuestion(t, db, ctx, lease)
				command = collaboration.Command{Kind: "answer", Target: round.Reference, Body: "Keep only nonpersonal decisions; the conflict still needs investigation."}
			}
			id := reviewReplanningInput(t, db, ctx, fixture.workflowID, command)
			terminal := store.MutationSpec{OperationID: "premature", ToolName: "request_replanning", Request: json.RawMessage(`{"operation_id":"premature"}`)}
			if _, err := db.ReserveMutation(ctx, lease, terminal); !errors.Is(err, store.ErrPendingHumanInput) {
				t.Fatal("undelivered input bypassed", err)
			}
			lease = reviewReplanningContinue(t, db, pool, ctx, lease, spec, workflow.TurnOutcomeClarificationWait)
			if _, err := db.ReserveMutation(ctx, lease, terminal); !errors.Is(err, store.ErrUnresolvedClarification) {
				t.Fatal("unassessed input bypassed", err)
			}
			disposition := "uncertain"
			if kind == "unresolved-question" {
				disposition = "unchanged"
			}
			reviewReplanningAssessment(t, db, ctx, lease, id, disposition)
			// Finishing implementation is still prohibited; transfer itself must not
			// require the Developer to resolve the Planner's material product question.
			ready := terminal
			ready.ToolName = "request_review"
			if _, err := db.ReserveMutation(ctx, lease, ready); !errors.Is(err, store.ErrUnresolvedClarification) {
				t.Fatal("uncertainty authorized implementation", err)
			}
			developer := lease.AgentParticipantID
			var references []string
			if kind == "uncertain-steering-referenced" {
				references = []string{id}
			}
			planner, _ := reviewReplanningConcern(t, db, pool, ctx, fixture, lease, spec, references...)
			state := replanningState(t, pool, ctx, fixture.workflowID)
			if state.ResumptionReady() || !state.AssessmentBlocked(planner.AgentParticipantID) && !state.Unresolved(planner.AgentParticipantID) {
				t.Fatal("transfer cleared material uncertainty")
			}
			for _, in := range state.Inputs {
				if in.ID == id && in.ParticipantID != developer {
					t.Fatal("input reparented", in)
				}
			}
			var charge int
			if err := pool.QueryRow(ctx, `SELECT autonomous_replanning_returns FROM workflow_attempts WHERE id=$1`, fixture.attemptID).Scan(&charge); err != nil || charge != 1 {
				t.Fatal(charge, err)
			}
		})
	}
}

func TestReplanningReviewLateInputReopensProvisionalReadiness(t *testing.T) {
	for i, scenario := range []struct {
		name, disposition string
		wait, withdraw    bool
	}{
		{name: "unchanged-fresh-consent", disposition: "unchanged"},
		{name: "revised", disposition: "revised"},
		{name: "explicit-wait-after-resolution", disposition: "revised", wait: true},
		{name: "withdrawal-after-resolution", disposition: "revised", withdraw: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			disposition := scenario.disposition
			dbs, pool := openPhaseFiveStores(t, 2)
			db := dbs[0]
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			fixture, developer, spec := reviewReplanningDeveloper(t, db, pool, ctx, 2470+i)
			lease, spec := reviewReplanningConcern(t, db, pool, ctx, fixture, developer, spec)
			state := replanningState(t, pool, ctx, fixture.workflowID)
			pair := *state.CurrentCheckpoint()
			content := pair.Content
			content.Changes = "Retain nonpersonal restart decisions"
			content.TechnicalPlan += " Persist only a nonpersonal decision enum."
			if disposition == "revised" {
				recordReplanningMutation(t, db, ctx, lease, "publish-revised", "publish_plan_checkpoint", map[string]any{"content": content}, func(op string) any {
					var err error
					pair, err = db.ReservePlanningCheckpoint(ctx, lease, op, content)
					if err != nil {
						t.Fatal(err)
					}
					if err = db.ConfirmPlanningPublication(ctx, fixture.workflowID, pair.ID, collaboration.Source{Channel: "fixture", ID: "revised"}); err != nil {
						t.Fatal(err)
					}
					return map[string]any{"revision_id": pair.ID}
				})
			}
			consent := reviewReplanningInput(t, db, ctx, fixture.workflowID, collaboration.Command{Kind: "implement", Target: pair.Reference})
			lease = reviewReplanningContinue(t, db, pool, ctx, lease, spec, workflow.TurnOutcomeClarificationWait)
			reviewReplanningAssessment(t, db, ctx, lease, consent, "confirmed")
			if _, err := db.RecordReadInvocation(ctx, lease, store.ReadInvocation{StartedAt: time.Now(), FinishedAt: time.Now(), ToolName: "refresh_default_branch", Request: json.RawMessage(`{"operation_id":"base"}`), Result: json.RawMessage(`{"status":"current","sha":"` + strings.Repeat("a", 40) + `"}`)}); err != nil {
				t.Fatal(err)
			}
			resolution := recordReplanningMutation(t, db, ctx, lease, "complete", "complete_planning", map[string]any{"revision_id": pair.ID, "repository_commit": strings.Repeat("a", 40), "evidence": "The isolated experiment supports the exact authorized commitments.", "disposition": disposition, "material_decisions_resolved": true}, func(op string) any {
				if err := db.CompleteReplanning(ctx, lease, op, pair.ID, strings.Repeat("a", 40), "The isolated experiment supports the exact authorized commitments.", disposition, true); err != nil {
					t.Fatal(err)
				}
				return map[string]any{"outcome": "REPLANNING_RESOLVED", "revision_id": pair.ID, "repository_commit": strings.Repeat("a", 40)}
			})
			state = replanningState(t, pool, ctx, fixture.workflowID)
			if state.Planning.ReadyRevision != pair.ID {
				t.Fatal("fixture did not establish provisional readiness")
			}
			late := ""
			if scenario.withdraw {
				reviewReplanningInput(t, dbs[1], ctx, fixture.workflowID, collaboration.Command{Kind: "withdraw-implementation", Target: pair.Reference})
				if err := db.CloseMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
				settled, err := db.SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomeReplanningResolved, nil))
				if err != nil || settled.SuccessorJobID != "" || settled.State != workflow.StatePlanning {
					t.Fatal("withdrawal did not wait", settled, err)
				}
				state = replanningState(t, pool, ctx, fixture.workflowID)
				if state.Planning.ReadyRevision != "" || state.ResumptionReady() || state.ImplementationGrant() != nil {
					t.Fatal("withdrawal retained provisional authority")
				}
				late = reviewReplanningInput(t, dbs[1], ctx, fixture.workflowID, collaboration.Command{Kind: "steer", Body: "Reconsider storage compatibility before continuing."})
				var jobID string
				if err = pool.QueryRow(ctx, `SELECT id::text FROM jobs WHERE workflow_id=$1 AND kind='PREPARE_AGENT_TURN' AND status='AVAILABLE' AND payload->>'role'='PLANNER'`, fixture.workflowID).Scan(&jobID); err != nil {
					t.Fatal(err)
				}
				job := claimSettlementSuccessorJob(t, db, pool, ctx, jobID, "post-withdrawal-planner")
				prepared, err := db.PrepareAgentTurn(ctx, *job, spec)
				if err != nil {
					t.Fatal(err)
				}
				lease = runPreparedReplanningTurn(t, db, pool, ctx, prepared)
			} else {
				late = reviewReplanningInput(t, dbs[1], ctx, fixture.workflowID, collaboration.Command{Kind: "steer", Body: "Reconsider storage compatibility before continuing."})
				outcome := workflow.TurnOutcomeReplanningResolved
				if scenario.wait {
					outcome = workflow.TurnOutcomeClarificationWait
				}
				lease = reviewReplanningContinue(t, db, pool, ctx, lease, spec, outcome)
			}
			state = replanningState(t, pool, ctx, fixture.workflowID)
			if state.Planning.ReadyRevision != "" || state.ActiveReplanning().Phase != collaboration.ReplanningActive || state.ResumptionReady() {
				t.Fatal("late input left provisional authority installed", state.Planning.ReadyRevision, state.ActiveReplanning())
			}
			if _, err := dbs[1].GetReplanningResolution(ctx, fixture.workflowID, resolution.ID); err != nil {
				t.Fatal("resolution receipt lost", err)
			}
			reviewReplanningAssessment(t, db, ctx, lease, late, "changed")
			content.TechnicalPlan += " Use a compatible on-disk format verified by the new repository experiment."
			recordReplanningMutation(t, db, ctx, lease, "replacement", "publish_plan_checkpoint", map[string]any{"content": content}, func(op string) any {
				replacement, err := db.ReservePlanningCheckpoint(ctx, lease, op, content)
				if err != nil {
					t.Fatal("replacement pair remained blocked after reassessment", err)
				}
				return map[string]any{"revision_id": replacement.ID}
			})
			var charge int
			if err := pool.QueryRow(ctx, `SELECT autonomous_replanning_returns FROM workflow_attempts WHERE id=$1`, fixture.attemptID).Scan(&charge); err != nil || charge != 1 {
				t.Fatal("reassessment changed charge", charge, err)
			}
		})
	}
}

func TestReplanningReviewCrossParticipantEffectsRecoverLostAcknowledgments(t *testing.T) {
	dbs, pool := openPhaseFiveStores(t, 2)
	db := dbs[0]
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fixture, developer, spec := reviewReplanningDeveloper(t, db, pool, ctx, 2480)
	round := reviewReplanningQuestion(t, db, ctx, developer)
	input := reviewReplanningInput(t, db, ctx, fixture.workflowID, collaboration.Command{Kind: "answer", Target: round.Reference, Body: "Only nonpersonal decisions may be retained."})
	developer = reviewReplanningContinue(t, db, pool, ctx, developer, spec, workflow.TurnOutcomeClarificationWait)
	reviewReplanningAssessment(t, db, ctx, developer, input, "unchanged")
	resolutions := []collaboration.Resolution{{QuestionID: "retention", ResponseIDs: []string{input}, Explanation: "The decision preserves the original retention commitment."}}
	recordReplanningMutation(t, db, ctx, developer, "original-interpretation", "resolve_clarification", map[string]any{"round_id": round.ID, "resolutions": resolutions}, func(op string) any {
		if err := db.ResolveClarification(ctx, developer, op, round.ID, resolutions); err != nil {
			t.Fatal(err)
		}
		return map[string]any{"recorded": true}
	})
	planner, _ := reviewReplanningConcern(t, db, pool, ctx, fixture, developer, spec)
	reserve := func(label, tool string, args any) store.MutationReservation {
		t.Helper()
		raw, _ := json.Marshal(args)
		var object map[string]json.RawMessage
		_ = json.Unmarshal(raw, &object)
		object["operation_id"], _ = json.Marshal(label)
		raw, _ = json.Marshal(object)
		m, err := db.ReserveMutation(ctx, planner, store.MutationSpec{OperationID: label, ToolName: tool, Request: raw, ExternalService: "omnigrex", ExternalResourceID: fixture.workflowID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.StartMutation(ctx, planner, m.ID); err != nil {
			t.Fatal(err)
		}
		return m
	}
	batch := []collaboration.Assessment{{InputID: input, EvidenceIDs: []string{input}, Disposition: "unchanged", Explanation: "The Planner independently assessed the Developer-owned answer."}}
	assessment := reserve("lost-assessment", "assess_human_input", map[string]any{"assessments": batch})
	if err := db.AssessHumanInput(ctx, planner, assessment.ID, batch); err != nil {
		t.Fatal(err)
	}
	resolutions[0].Obsolete = true
	resolutions[0].Explanation = "The exact accepted pair already settles the retention boundary; this historical question is obsolete."
	resolution := reserve("lost-resolution", "resolve_clarification", map[string]any{"round_id": round.ID, "resolutions": resolutions})
	if err := db.ResolveClarification(ctx, planner, resolution.ID, round.ID, resolutions); err != nil {
		t.Fatal(err)
	}
	found, err := dbs[1].HasHumanInputAssessmentEffect(ctx, fixture.workflowID, assessment.ID, planner.AgentParticipantID, batch)
	if err != nil || !found {
		t.Error("committed cross-Participant assessment not recovered", found, err)
	}
	// An actual effect is not proof for another operation or Participant.
	for _, op := range []string{resolution.ID, uuid.NewString()} {
		found, err = dbs[1].HasHumanInputAssessmentEffect(ctx, fixture.workflowID, op, planner.AgentParticipantID, batch)
		if err != nil || found {
			t.Fatal("unrelated operation accepted", found, err)
		}
	}
	found, err = dbs[1].HasHumanInputAssessmentEffect(ctx, fixture.workflowID, assessment.ID, developer.AgentParticipantID, batch)
	if err != nil || found {
		t.Fatal("wrong operation owner accepted", found, err)
	}
	found, err = dbs[1].HasClarificationResolutionEffect(ctx, fixture.workflowID, resolution.ID, planner.AgentParticipantID, round.ID, resolutions)
	if err != nil || !found {
		t.Fatal("committed cross-Participant interpretation not found", found, err)
	}
	for _, op := range []string{assessment.ID, uuid.NewString()} {
		found, err = dbs[1].HasClarificationResolutionEffect(ctx, fixture.workflowID, op, planner.AgentParticipantID, round.ID, resolutions)
		if err != nil || found {
			t.Fatal("other operation proved resolution", found, err)
		}
	}
	found, err = dbs[1].HasClarificationResolutionEffect(ctx, fixture.workflowID, resolution.ID, developer.AgentParticipantID, round.ID, resolutions)
	if err != nil || found {
		t.Fatal("original ownership substituted for operation ownership", found, err)
	}
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &replayGatewayGitHub{}, Credentials: replayGatewayCredentials{}, Publications: unusedHandoffPublications{}, Clarifications: dbs[1]})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := db.GetAgentTurnExecutionContext(ctx, planner)
	if err != nil {
		t.Fatal(err)
	}
	probe := resolution
	probe.State = store.MutationUnknown
	proof, err := reconciler.Reconcile(ctx, store.AgentTurnMutationReconciliationContext{WorkflowID: fixture.workflowID, Repository: execution.Repository, Issue: execution.Issue, Role: workflow.RolePlanner, Turn: execution.Turn}, probe)
	if err != nil || proof.Disposition != mcp.ReconciliationFound {
		t.Fatal("production resolution reconciler rejected committed cycle-scoped effect", proof, err)
	}
	if err = db.CloseMutationAdmissionWithPromptEvidence(ctx, planner, "completed", ""); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, planner.JobLease.ID, planner.ID)
	if _, err = db.RecoverExpiredAgentTurn(ctx, planner.ID, planner.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	stop := claimRecoveryJob(t, db, ctx, store.StopStaleRuntimeJobKind, "cross-scope-stop")
	if _, err = db.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	worker, err := mcp.NewRecoveryWorker(dbs[1], reconciler, mcp.RecoveryWorkerConfig{ClaimOwner: "cross-scope-recovery", LeaseDuration: time.Minute, HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if processed, e := worker.ProcessNext(ctx); e != nil || !processed {
		t.Fatal(processed, e)
	}
	if processed, e := worker.ProcessNext(ctx); e != nil || processed {
		t.Fatal("duplicate recovery", processed, e)
	}
	for _, op := range []string{assessment.ID, resolution.ID} {
		var status string
		if err = pool.QueryRow(ctx, `SELECT state FROM tool_invocations WHERE id=$1`, op).Scan(&status); err != nil || status != "SUCCEEDED" {
			t.Fatal("committed effect not recovered", status, err)
		}
	}
	state := replanningState(t, pool, ctx, fixture.workflowID)
	for _, in := range state.Inputs {
		if in.ID == input && in.ParticipantID != developer.AgentParticipantID {
			t.Fatal("recovery reparented input")
		}
	}
	restored, err := dbs[1].GetClarificationRound(ctx, fixture.workflowID, round.ID)
	if err != nil || restored.ParticipantID != developer.AgentParticipantID || len(restored.Resolutions) != 2 {
		t.Fatal("recovery duplicated/reparented interpretation", restored, err)
	}
}

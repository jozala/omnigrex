//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestHumanInputPresentationRacesRetainContinuation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for n, role := range []workflow.Role{workflow.RolePlanner, workflow.RoleDeveloper, workflow.RoleReviewer} {
		for m, mode := range []string{"success", "unknown-outcome", "expired", "late-expired"} {
			t.Run(fmt.Sprintf("%s/%s", role, mode), func(t *testing.T) {
				database := databases[0]
				head := "current-head"
				if role == workflow.RolePlanner {
					head = ""
				}
				fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 1200+10*n+m, role, head)
				request := json.RawMessage(`{"operation_id":"question","questions":[{"id":"Q1","body":"Compatibility?"},{"id":"Q2","body":"Retention?"}]}`)
				mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{ToolName: "request_clarification", OperationID: "question", Request: request, ExternalService: "github", ExternalResourceID: "fixture"})
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
				if err = database.ConfirmClarificationPublication(ctx, fixture.workflowID, round.ID, collaboration.Source{Channel: "fixture", ID: "published"}); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(map[string]any{"outcome": "CLARIFICATION_WAIT", "round_id": round.ID})
				if err = database.CompleteMutation(ctx, lease, mutation.ID, raw); err != nil {
					t.Fatal(err)
				}
				if err = database.CloseMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
				settled, err := database.SettleAgentTurn(ctx, lease, store.AgentTurnSettlementObservation{ObservedAt: time.Now(), Outcome: workflow.TurnOutcomeClarificationWait, Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded}})
				if err != nil {
					t.Fatal(err)
				}
				if settled.SuccessorJobID != "" {
					t.Fatal("autonomous continuation during wait")
				}
				// Isolate this overlay test from Stage-label fixture setup jobs.
				if _, err = pool.Exec(ctx, `UPDATE jobs SET status='CANCELLED',completed_at=clock_timestamp() WHERE kind='RECONCILE_GITHUB_LABELS' AND status='AVAILABLE' AND COALESCE((payload->>'human_input_only')::boolean,false)=false`); err != nil {
					t.Fatal(err)
				}
				var labelJob string
				if err = pool.QueryRow(ctx, `SELECT id::text FROM jobs WHERE workflow_id=$1 AND idempotency_key LIKE 'human-input-presentation:%' ORDER BY created_at DESC LIMIT 1`, fixture.workflowID).Scan(&labelJob); err != nil {
					t.Fatal(err)
				}
				if err = prioritizeFixtureJob(pool, ctx, labelJob, store.WorkflowActionQueue, store.ReconcileGitHubLabelsJobKind); err != nil {
					t.Fatal(err)
				}
				input := store.HumanInputSubmission{WorkflowID: fixture.workflowID, Command: collaboration.Command{Kind: "answer", Target: round.Reference, Body: "Preserve compatibility; retention undecided."}, Actor: collaboration.Actor{ID: "maintainer"}, Source: collaboration.Source{Channel: "fixture", ID: "reply"}, Authorized: true, At: time.Now()}
				// Invalid commands do not change the genuine wait or allocate continuation.
				invalid := input
				invalid.Source.ID = "invalid"
				invalid.Command.Target = "R999"
				if receipt, err := databases[1].AdmitHumanInput(ctx, invalid); err != nil || receipt.ResponseID != "" {
					t.Fatal(receipt, err)
				}
				var beforeVersion int64
				var visible bool
				if err = pool.QueryRow(ctx, `SELECT human_input_waiting,human_input_presentation_revision FROM workflows WHERE id=$1`, fixture.workflowID).Scan(&visible, &beforeVersion); err != nil || !visible || beforeVersion != 1 {
					t.Fatal(visible, beforeVersion, err)
				}
				admit := func() {
					t.Helper()
					if receipt, err := databases[1].AdmitHumanInput(ctx, input); err != nil || receipt.ResponseID == "" {
						t.Fatal(receipt, err)
					}
				}
				api := &inputOverlayAPI{}
				if mode == "expired" {
					old, err := database.ClaimWorkflowGitHubEffectJob(ctx, store.ReconcileGitHubLabelsJobKind, "old-worker", time.Minute)
					if err != nil || old == nil || old.ID != labelJob {
						t.Fatal(old, err)
					}
					observed, err := database.GetWorkflowGitHubEffectContext(ctx, *old)
					if err != nil || !observed.HumanInputWaiting {
						t.Fatal(observed, err)
					}
					// A completed remote write with lost acknowledgement survives process death.
					api.present = true
					admit()
					if _, err = pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, old.ID); err != nil {
						t.Fatal(err)
					}
					if _, err = database.AcknowledgeWorkflowGitHubEffect(ctx, *old, observed, json.RawMessage(`{}`)); !errors.Is(err, store.ErrWorkflowGitHubEffectFenceLost) {
						t.Fatal(err)
					}
					forged := *old
					forged.LeaseToken = "00000000-0000-4000-8000-000000000001"
					if err = database.EnsureWorkflowGitHubLabelCorrection(ctx, forged); !errors.Is(err, store.ErrWorkflowGitHubEffectFenceLost) {
						t.Fatal("unproven correction", err)
					}
					for i := 0; i < 2; i++ {
						if err = databases[1].EnsureWorkflowGitHubLabelCorrection(ctx, *old); err != nil {
							t.Fatal(err)
						}
					}
					var retained int
					if err = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id=$1 AND idempotency_key LIKE 'human-input-lost-lease:%'`, fixture.workflowID).Scan(&retained); err != nil || retained != 1 {
						t.Fatal(retained, err)
					}

				} else {
					// Deterministic barrier: admission occurs inside the remote add, after
					// the worker's durable observation and before acknowledgement.
					if mode != "late-expired" {
						api.onAdd = admit
					}
					api.unknown = mode == "unknown-outcome"
				}
				worker, err := githubapi.NewLabelWorker(databases[1], handoffFixtureCredential{}, api, githubapi.VisibleEffectWorkerConfig{ClaimOwner: "new-worker", LeaseDuration: time.Minute, IdlePollInterval: time.Millisecond, HeartbeatInterval: time.Second, RetryDelay: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				if mode == "late-expired" {
					api.beforeAdd = func() {
						admit()
						if _, err = pool.Exec(ctx, `UPDATE jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, labelJob); err != nil {
							t.Fatal(err)
						}
						for n := 0; n < 10; n++ {
							done, err := worker.ProcessNext(ctx)
							if err != nil {
								t.Fatal(err)
							}
							if !done {
								break
							}
							if n == 9 {
								t.Fatal("unbounded newer-worker processing")
							}
						}
					}
				}
				processed, processErr := worker.ProcessNext(ctx)
				if !processed || (mode != "late-expired" && processErr != nil) || (mode == "late-expired" && processErr == nil) {
					t.Fatal(processed, processErr)
				}
				api.beforeAdd = nil
				if mode == "late-expired" {
					var corrections int
					if err = pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id=$1 AND idempotency_key LIKE 'human-input-lost-lease:%' AND status='AVAILABLE'`, fixture.workflowID).Scan(&corrections); err != nil || corrections != 1 {
						t.Fatal(corrections, err)
					}
				}
				api.onAdd = nil
				api.unknown = false
				// Drain ordinary durable corrections after the stale add/unknown response.
				for i := 0; i < 20; i++ {
					processed, err := worker.ProcessNext(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if !processed {
						break
					}
					if i == 19 {
						t.Fatal("unbounded reconciliation")
					}
				}
				if api.present {
					t.Fatal("obsolete waiting label survived")
				}
				admit() // source replay must not queue another continuation
				var version, stateRevision int64
				var prepares, turns, failures, reviews int
				var rawState []byte
				if err = pool.QueryRow(ctx, `SELECT w.human_input_waiting,w.human_input_presentation_revision,w.state_revision,(SELECT count(*) FROM jobs WHERE workflow_id=w.id AND kind='PREPARE_AGENT_TURN' AND status IN ('AVAILABLE','LEASED')),(SELECT count(*) FROM agent_turns WHERE workflow_id=w.id),a.infrastructure_failures,COALESCE((a.review_usage->>'review')::int,0),c.state FROM workflows w JOIN workflow_attempts a ON a.id=$2 JOIN work_item_collaboration c ON c.workflow_id=w.id WHERE w.id=$1`, fixture.workflowID, fixture.attemptID).Scan(&visible, &version, &stateRevision, &prepares, &turns, &failures, &reviews, &rawState); err != nil {
					t.Fatal(err)
				}
				var state collaboration.State
				if err = json.Unmarshal(rawState, &state); err != nil {
					t.Fatal(err)
				}
				if visible || version != 2 || stateRevision != int64(settled.Revision) || prepares != 1 || turns != 1 || failures != 0 || reviews != 0 || len(state.Inputs) != 1 || state.Inputs[0].ParticipantID != fixture.assignmentID || state.Inputs[0].DeliveredTo != "" {
					t.Fatalf("continuity changed: wait=%t v=%d state=%d jobs=%d turns=%d retries=%d reviews=%d inputs=%+v", visible, version, stateRevision, prepares, turns, failures, reviews, state.Inputs)
				}

				if role == workflow.RoleDeveloper && mode == "success" {
					var prepareID string
					if err = pool.QueryRow(ctx, `SELECT id::text FROM jobs WHERE workflow_id=$1 AND kind='PREPARE_AGENT_TURN' AND status='AVAILABLE'`, fixture.workflowID).Scan(&prepareID); err != nil {
						t.Fatal(err)
					}
					next := claimSettlementSuccessorJob(t, database, pool, ctx, prepareID, "continue-input")
					original := fixture.turnSpec()
					spec := store.AgentTurnPreparationSpec{Stages: map[workflow.StageID]store.ParticipantPreparation{workflow.StageImplementation: {ProfilePath: ".omnigrex/team/developer.md", Binding: store.ParticipantRuntimeBinding{AgentProfileName: "developer", RuntimeProfileName: "pi-rpc", RuntimeProfileVersion: "v2", RuntimeProfileContentSHA256: strings.Repeat("a", 64), RuntimeImageDigest: "sha256:test"}, Profile: store.AgentProfileSnapshot{CommitSHA: original.AgentProfileCommitSHA, ContentSHA256: original.AgentProfileContentSHA256, Config: original.AgentProfileConfig}}}}
					prepared, err := prepareWithFixtureAdmission(t, database, ctx, *next, spec)
					if err != nil {
						t.Fatal(err)
					}
					if prepared.Participant.ID != fixture.assignmentID || prepared.Session.ID != fixture.sessionID || prepared.Turn.WorkflowAttemptID != fixture.attemptID {
						t.Fatal("continuation changed ownership")
					}
					nextLease, err := claimAndAcquireFixtureAgentTurn(t, database, pool, ctx, prepared.Turn, "continued-runtime", time.Minute, 100)
					if err != nil {
						t.Fatal(err)
					}
					if err = database.OpenMutationAdmission(ctx, nextLease); err != nil {
						t.Fatal(err)
					}
					mutation, err := database.ReserveMutation(ctx, nextLease, store.MutationSpec{ToolName: "request_clarification", OperationID: "next-question", Request: json.RawMessage(`{"operation_id":"next-question","questions":[{"id":"Q1","body":"Compatibility?"},{"id":"Q2","body":"Retention?"},{"id":"Q3","body":"Retention choice?"}]}`), ExternalService: "github", ExternalResourceID: "fixture"})
					if err != nil {
						t.Fatal(err)
					}
					if _, err = database.StartMutation(ctx, nextLease, mutation.ID); err != nil {
						t.Fatal(err)
					}
					nextRound, err := database.RequestClarification(ctx, nextLease, mutation.ID, []collaboration.Question{{ID: "Q1", Body: "Compatibility?"}, {ID: "Q2", Body: "Retention?"}, {ID: "Q3", Body: "Retention choice?"}})
					if err != nil {
						t.Fatal(err)
					}
					if err = database.ConfirmClarificationPublication(ctx, fixture.workflowID, nextRound.ID, collaboration.Source{Channel: "fixture", ID: "new-publication"}); err != nil {
						t.Fatal(err)
					}
					raw, _ := json.Marshal(map[string]any{"outcome": "CLARIFICATION_WAIT", "round_id": nextRound.ID})
					if err = database.CompleteMutation(ctx, nextLease, mutation.ID, raw); err != nil {
						t.Fatal(err)
					}
					if err = database.CloseMutationAdmission(ctx, nextLease); err != nil {
						t.Fatal(err)
					}
					if _, err = database.SettleAgentTurn(ctx, nextLease, store.AgentTurnSettlementObservation{ObservedAt: time.Now(), Outcome: workflow.TurnOutcomeClarificationWait, Completion: store.AgentTurnCompletion{Status: store.AgentTurnSucceeded}}); err != nil {
						t.Fatal(err)
					}
					admit() // replay of the old partial answer cannot clear the restored wait
					if err = pool.QueryRow(ctx, `SELECT human_input_waiting,human_input_presentation_revision FROM workflows WHERE id=$1`, fixture.workflowID).Scan(&visible, &version); err != nil || !visible || version != 3 {
						t.Fatal(visible, version, err)
					}
					closeWorkflowForLiveness(t, database, ctx, fixture, 1200+10*n+m)
					if err = pool.QueryRow(ctx, `SELECT human_input_waiting,human_input_presentation_revision FROM workflows WHERE id=$1`, fixture.workflowID).Scan(&visible, &version); err != nil || visible || version != 4 {
						t.Fatal(visible, version, err)
					}
				}

			})
		}
	}
}

type inputOverlayAPI struct {
	present, unknown bool
	onAdd            func()
	beforeAdd        func()
}

func (a *inputOverlayAPI) ListRepositoryLabels(context.Context, string, string, string) ([]githubapi.Label, error) {
	return []githubapi.Label{{Name: githubapi.WaitingForInput}}, nil
}
func (a *inputOverlayAPI) CreateRepositoryLabel(context.Context, string, string, string, githubapi.Label) (githubapi.Label, error) {
	return githubapi.Label{}, errors.New("unexpected label creation")
}
func (a *inputOverlayAPI) ListIssueLabels(context.Context, string, string, string, int) ([]githubapi.Label, error) {
	if a.present {
		return []githubapi.Label{{Name: githubapi.WaitingForInput}}, nil
	}
	return nil, nil
}
func (a *inputOverlayAPI) AddIssueLabels(_ context.Context, _, _, _ string, _ int, names []string) ([]githubapi.Label, error) {
	if a.beforeAdd != nil {
		a.beforeAdd()
	}
	if len(names) != 1 || names[0] != githubapi.WaitingForInput {
		return nil, errors.New("unexpected Stage mutation")
	}
	a.present = true
	if a.onAdd != nil {
		a.onAdd()
	}
	if a.unknown {
		return nil, errors.New("lost add response")
	}
	return nil, nil
}
func (a *inputOverlayAPI) RemoveIssueLabel(_ context.Context, _, _, _ string, _ int, name string) error {
	if name != githubapi.WaitingForInput {
		return errors.New("unexpected Stage removal")
	}
	a.present = false
	return nil
}
func (a *inputOverlayAPI) GetPullRequest(context.Context, string, string, string, int) (githubapi.PullRequest, error) {
	return githubapi.PullRequest{}, errors.New("unexpected PR read")
}

func TestHumanInputUpgradeReconciliationUsesCurrentRecords(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for i, status := range []string{"DEVELOPING", "QUEUED", "CLOSED", "CLOSING", "DORMANT", "NEEDS_HUMAN", "PLAN_READY", "PR_READY"} {
		t.Run(status, func(t *testing.T) {
			fixture := seedAgentSession(t, pool, 1300+i)
			current := status
			if status == "QUEUED" {
				current = "DEVELOPING"
			}
			if _, err := pool.Exec(ctx, `UPDATE workflows SET status=$2,state_revision=7,clarification_waiting=true WHERE id=$1`, fixture.workflowID, current); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `INSERT INTO stage_assignments(workflow_id,assignment_generation,stage_id,role,agent_participant_id) VALUES($1,1,'implementation','DEVELOPER',$2)`, fixture.workflowID, fixture.assignmentID); err != nil {
				t.Fatal(err)
			}
			state := collaboration.State{Waiting: true, CurrentRound: "R1", Rounds: []collaboration.Round{{ID: "R1", ParticipantID: fixture.assignmentID, Published: true, Source: &collaboration.Source{Channel: "fixture", ID: "published"}, Questions: []collaboration.Question{{ID: "Q1", Body: "Policy?"}}}}}
			raw, _ := json.Marshal(state)
			if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
				t.Fatal(err)
			}
			if status == "QUEUED" {
				insertJob(t, pool, ctx, jobSeed{Queue: store.WorkflowActionQueue, Kind: store.PrepareAgentTurnJobKind, Payload: json.RawMessage(`{"revision":7}`), MaxAttempts: 3, IdempotencyKey: "queued:" + fixture.workflowID, WorkflowID: fixture.workflowID})
			}
			job := insertJob(t, pool, ctx, jobSeed{Queue: store.WorkflowActionQueue, Kind: store.ReconcileGitHubLabelsJobKind, Payload: json.RawMessage(`{"revision":7,"human_input_only":true}`), MaxAttempts: 3, IdempotencyKey: "human-input-upgrade:" + fixture.workflowID, WorkflowID: fixture.workflowID})
			if err := prioritizeFixtureJob(pool, ctx, job, store.WorkflowActionQueue, store.ReconcileGitHubLabelsJobKind); err != nil {
				t.Fatal(err)
			}
			api := &inputOverlayAPI{present: true}
			worker, err := githubapi.NewLabelWorker(database, handoffFixtureCredential{}, api, githubapi.VisibleEffectWorkerConfig{ClaimOwner: "upgrade-worker", LeaseDuration: time.Minute, HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, RetryDelay: time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			for n := 0; n < 4; n++ {
				done, err := worker.ProcessNext(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if !done {
					break
				}
				if n == 3 {
					t.Fatal("unbounded upgrade jobs")
				}
			}
			if api.present != (status == "DEVELOPING") {
				t.Fatalf("stale upgrade projection for %s: %t", status, api.present)
			}
			var turns, preparations int
			var history []byte
			if err = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM agent_turns WHERE workflow_id=$1),(SELECT count(*) FROM jobs WHERE workflow_id=$1 AND kind='PREPARE_AGENT_TURN'),state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&turns, &preparations, &history); err != nil {
				t.Fatal(err)
			}
			var preserved collaboration.State
			if err = json.Unmarshal(history, &preserved); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(state)
			after, _ := json.Marshal(preserved)
			wantPrepares := 0
			if status == "QUEUED" {
				wantPrepares = 1
			}
			if turns != 0 || preparations != wantPrepares || string(before) != string(after) {
				t.Fatal("upgrade allocated work or rewrote history")
			}
		})
	}
}

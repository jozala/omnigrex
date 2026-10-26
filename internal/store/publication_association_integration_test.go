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

	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/publication"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestInitialPublicationAssociationWithoutReadiness(t *testing.T) {
	for _, test := range []struct {
		name, mode                   string
		outcome                      workflow.TurnOutcome
		noEvents, omitLast, lostOpen bool
	}{
		{name: "blocked", outcome: workflow.TurnOutcomeBlocked},
		{name: "failed", outcome: workflow.TurnOutcomeInfrastructureFailed},
		{name: "interrupted", mode: "recovery", outcome: workflow.TurnOutcomeInfrastructureFailed},
		{name: "recovered unknown open", mode: "recovery", outcome: workflow.TurnOutcomeInfrastructureFailed, lostOpen: true},
		{name: "verifier handoff", mode: "handoff", outcome: workflow.TurnOutcomeInfrastructureFailed},
		{name: "verifier readiness", mode: "verified", outcome: workflow.TurnOutcomeChangeProposalReady},
		{name: "missing final webhook", outcome: workflow.TurnOutcomeBlocked, omitLast: true},
		{name: "no webhooks", outcome: workflow.TurnOutcomeBlocked, noEvents: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			outcome := test.outcome
			databases, pool := openPhaseFiveStores(t, 2)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			fixture := seedAgentSession(t, pool, 116)
			if _, err := pool.Exec(ctx, `UPDATE workflows SET status='DEVELOPING', state_revision=1, desired_assignment_status='ACTIVE', desired_runtime_state='ACTIVE' WHERE id=$1`, fixture.workflowID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit=1, current_stage='implementation' WHERE id=$1`, fixture.attemptID); err != nil {
				t.Fatal(err)
			}
			_, lease, execution := acquireReplayGatewayTurn(t, database, pool, fixture, store.AgentTurn{}, "association")
			const base = "0123456789abcdef0123456789abcdef01234567"
			heads := []string{"1123456789abcdef0123456789abcdef01234567", "2123456789abcdef0123456789abcdef01234567", "3123456789abcdef0123456789abcdef01234567"}
			api := &replayGatewayGitHub{head: heads[0], branch: "omnigrex/issue-116", base: "main"}
			publisher := &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: heads[0], Changed: true}, {Head: heads[1], Changed: true}, {Head: heads[2], Changed: true}}}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: replayGatewayCredentials{}, Publisher: publisher, Workflow: mcp.LedgerWorkflowMutations{}})
			if err != nil {
				t.Fatal(err)
			}
			gateway, registration := openReplayGateway(t, database, backend, replayGatewayScope(lease, execution, base))
			callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1"}, false)
			callReplayGatewayTool(t, gateway, registration, mcp.ToolOpenPR, map[string]any{"operation_id": "open", "title": "Changes", "body": "Not ready"}, false)
			callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-2"}, false)
			callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-3"}, false)
			for index, event := range []struct{ action, before, head string }{
				{"opened", "", heads[0]}, {"synchronize", heads[1], heads[2]}, {"synchronize", heads[0], heads[1]}, {"synchronize", heads[0], heads[1]},
			} {
				if test.noEvents || test.omitLast && index == 1 {
					continue
				}
				deliveryID := fmt.Sprintf("11600000-0000-4000-8000-%012d", index+1)
				claim := claimWorkflowDelivery(t, database, ctx, store.WebhookDelivery{DeliveryID: deliveryID, EventName: "pull_request", Action: event.action, RepositoryID: execution.Repository.ID, RepositoryOwner: execution.Repository.Owner, RepositoryName: execution.Repository.Name, Headers: map[string]string{}, Payload: []byte(`{}`)})
				payload, err := json.Marshal(map[string]any{"delivery_id": deliveryID, "event": "pull_request", "action": event.action, "repository": map[string]any{"id": execution.Repository.ID, "owner": execution.Repository.Owner, "name": execution.Repository.Name}, "pull_request": map[string]any{"id": 654, "number": 23, "head_sha": event.head, "before_sha": event.before, "head_ref": api.branch, "base_ref": "main", "base_sha": api.pullRequest().Base.SHA, "node_id": "PR_654"}})
				if err != nil {
					t.Fatal(err)
				}
				locator := store.WorkflowLocator{WorkflowID: execution.WorkflowID, RepositoryID: execution.Repository.ID, PullRequestID: 654, PullRequestNumber: 23}
				application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, payload, locator, func(c store.WorkflowEventContext) (workflow.Event, error) {
					if event.action == "opened" {
						return workflow.ChangeProposalObservedEvent{EventMetadata: c.Metadata, ChangeProposal: workflow.ChangeProposal{ID: 654, Number: 23, HeadSHA: event.head, Open: true}}, nil
					}
					return workflow.SynchronizationEvent{EventMetadata: c.Metadata, ChangeProposalID: 654, PreviousHeadSHA: event.before, HeadSHA: event.head}, nil
				})
				if err != nil || application.Status != store.NormalizedEventDeferred {
					t.Fatalf("defer %s = %#v, %v", event.action, application, err)
				}
			}
			if test.mode == "handoff" || test.mode == "verified" {
				api.head = heads[2]
				callReplayGatewayTool(t, gateway, registration, mcp.ToolRequestReview, map[string]any{"operation_id": "ready", "summary": "Ready"}, false)
			}
			if outcome == workflow.TurnOutcomeBlocked {
				callReplayGatewayTool(t, gateway, registration, mcp.ToolReportBlocked, map[string]any{"operation_id": "blocked", "reason": "Required fixture unavailable"}, false)
			}
			if err := gateway.CloseAndDrain(ctx, registration); err != nil {
				t.Fatal(err)
			}
			// Settlement/replay uses a distinct store instance; no backend map survives.
			database = databases[1]
			var settled store.AgentTurnSettlement
			var errSettle error
			if test.mode == "handoff" || test.mode == "verified" {
				if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
					t.Fatal(err)
				}
				ledger, err := database.ListAgentTurnMutationInvocations(ctx, lease)
				if err != nil {
					t.Fatal(err)
				}
				var source string
				for _, m := range ledger {
					if m.ToolName == mcp.ToolRequestReview {
						source = m.ID
					}
				}
				prompt := json.RawMessage(`{"admitted":true,"status":"completed"}`)
				if _, err := database.BeginTerminalCorroboration(ctx, lease, source, prompt, "", "transient_transport"); err != nil {
					t.Fatal(err)
				}
				verifier, err := database.ClaimJobKind(ctx, "agent-turn-recovery", store.VerifyTerminalIntentJobKind, "association-verifier", time.Minute)
				if err != nil || verifier == nil {
					t.Fatalf("verifier = %#v, %v", verifier, err)
				}
				if test.mode == "handoff" {
					settled, errSettle = database.CompleteTerminalCorroborationHandoff(ctx, *verifier, workflow.ReasonTerminalCorroborationExhausted, "transient_transport")
					if errSettle == nil {
						again, err := databases[0].CompleteTerminalCorroborationHandoff(ctx, *verifier, workflow.ReasonTerminalCorroborationExhausted, "transient_transport")
						if err != nil || again != settled {
							t.Fatalf("handoff replay = %#v, %v", again, err)
						}
					}
				} else {
					proposal := &store.AgentTurnSettlementChangeProposal{WorkflowID: execution.WorkflowID, RepositoryID: execution.Repository.ID, RepositoryOwner: execution.Repository.Owner, RepositoryName: execution.Repository.Name, PullRequestID: 654, PullRequestNumber: 23, PullRequestNodeID: "PR_654", Status: "OPEN", Active: true, BaseRef: "main", BaseSHA: api.pullRequest().Base.SHA, HeadRef: api.branch, HeadSHA: heads[2]}
					// A normal default-branch advance is not a PR identity change.
					proposal.BaseSHA = base
					observation := successfulSettlementObservation(workflow.TurnOutcomeChangeProposalReady, proposal)
					observation.Completion.Outcome = prompt
					settled, errSettle = database.SettleTerminalCorroboration(ctx, *verifier, observation)
				}
			} else {
				if err := database.CloseMutationAdmission(ctx, lease); err != nil {
					t.Fatal(err)
				}
				if test.mode == "recovery" {
					if test.lostOpen {
						// GitHub succeeded, but the successful local result was not retained.
						api.head = heads[2]
						if _, err := pool.Exec(ctx, `UPDATE tool_invocations SET state='UNKNOWN',result=NULL WHERE agent_turn_id=$1 AND tool_name='open_pr'`, lease.ID); err != nil {
							t.Fatal(err)
						}
					}
					for _, expiration := range []struct {
						query string
						args  []any
					}{
						{`UPDATE jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, []any{lease.JobLease.ID}},
						{`UPDATE job_attempts SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE job_id=$1 AND attempt_number=$2`, []any{lease.JobLease.ID, lease.JobLease.Attempt}},
						{`UPDATE agent_turns SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, []any{lease.ID}},
						{`UPDATE agent_turn_slots SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE agent_turn_id=$1`, []any{lease.ID}},
					} {
						if _, err := pool.Exec(ctx, expiration.query, expiration.args...); err != nil {
							t.Fatal(err)
						}
					}
					recovery, err := database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch)
					if err != nil {
						t.Fatal(err)
					}
					if recovery.ReconcileMutationsJobID == "" {
						t.Fatal("publication recovery did not require mutation barrier")
					}
					mutationLease := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "association-mutations")
					if _, err := database.CompleteAgentTurnMutationReconciliation(ctx, mutationLease); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
						t.Fatalf("mutation completion before stop = %v", err)
					}
					stopLease := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "association-stop")
					stopped, err := database.AcknowledgeRecoveredRuntimeStopped(ctx, stopLease)
					if err != nil {
						t.Fatal(err)
					}
					if stopped.SettlementID != "" {
						t.Fatal("stop bypassed the mutation barrier")
					}
					if test.lostOpen {
						if _, err := database.CompleteAgentTurnMutationReconciliation(ctx, mutationLease); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
							t.Fatalf("unresolved open bypassed barrier: %v", err)
						}
						reconciliation, err := database.GetAgentTurnMutationReconciliationContext(ctx, mutationLease)
						if err != nil {
							t.Fatal(err)
						}
						mutations, err := database.ListAgentTurnMutationsForReconciliation(ctx, mutationLease)
						if err != nil {
							t.Fatal(err)
						}
						reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: api, Credentials: replayGatewayCredentials{}, Publications: unusedHandoffPublications{}})
						if err != nil {
							t.Fatal(err)
						}
						for _, m := range mutations {
							if m.ToolName == mcp.ToolOpenPR {
								recovered, err := reconciler.Reconcile(ctx, reconciliation, m)
								if err != nil || recovered.Disposition != mcp.ReconciliationFound {
									t.Fatalf("recover open = %#v, %v", recovered, err)
								}
								result, err := publication.DecodeOpenResult(recovered.Outcome.Result)
								if err != nil || result.HeadSHA != heads[0] {
									t.Fatalf("recovery replaced operation head: %#v, %v", result, err)
								}
								if _, err := database.ReconcileRecoveredMutation(ctx, mutationLease, m.ID, recovered.Outcome); err != nil {
									t.Fatal(err)
								}
							}
						}
					}

					completed, err := database.CompleteAgentTurnMutationReconciliation(ctx, mutationLease)
					if err != nil {
						t.Fatal(err)
					}
					var raw []byte
					if err := pool.QueryRow(ctx, `SELECT result FROM agent_turn_settlements WHERE id=$1`, completed.SettlementID).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(raw, &settled); err != nil {
						t.Fatal(err)
					}
					again, err := databases[0].CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
					if err != nil || again.SettlementID != settled.ID {
						t.Fatalf("recovery replay = %#v, %v", again, err)
					}
				} else {
					observation := failedSettlementObservation("original runtime failure")
					if outcome == workflow.TurnOutcomeBlocked {
						observation = successfulSettlementObservation(outcome, nil)
						observation.Diagnostic = "Required fixture unavailable"
					}
					settled, errSettle = database.SettleAgentTurn(ctx, lease, observation)
					if errSettle == nil {
						replay, err := databases[0].SettleAgentTurn(ctx, lease, observation)
						if err != nil || replay != settled {
							t.Fatalf("settlement replay = %#v, %v", replay, err)
						}
					}
				}
			}
			if errSettle != nil {
				t.Fatal(errSettle)
			}
			if settled.ChangeProposalID == "" {
				t.Fatal("successful open lost its durable Workflow association on non-ready settlement")
			}

			var head, ready, turnProposal, turnHead string
			if err := pool.QueryRow(ctx, `SELECT proposal.head_sha,COALESCE(proposal.ready_for_sha,''),COALESCE(turn.change_proposal_id::text,''),COALESCE(turn.expected_head_sha,'') FROM change_proposals AS proposal JOIN agent_turns AS turn ON turn.id=$2 WHERE proposal.id=$1`, settled.ChangeProposalID, lease.ID).Scan(&head, &ready, &turnProposal, &turnHead); err != nil {
				t.Fatal(err)
			}
			if head != heads[2] || ready != "" || turnProposal != "" || turnHead != lease.ExpectedHeadSHA {
				t.Fatalf("association changed readiness or authority: %s / %s / %s / %s", head, ready, turnProposal, turnHead)
			}
			var used int
			var diagnostic string
			if err := pool.QueryRow(ctx, `SELECT attempt.infrastructure_failures,COALESCE(settlement.terminal_last_error,'') FROM workflow_attempts AS attempt JOIN agent_turn_settlements AS settlement ON settlement.workflow_attempt_id=attempt.id WHERE settlement.id=$1`, settled.ID).Scan(&used, &diagnostic); err != nil {
				t.Fatal(err)
			}
			wantUsed := 0
			if outcome == workflow.TurnOutcomeInfrastructureFailed && test.mode != "handoff" {
				wantUsed = 1
			}
			if used != wantUsed {
				t.Fatalf("association changed retry budget: %d, want %d", used, wantUsed)
			}
			if outcome == workflow.TurnOutcomeInfrastructureFailed && test.mode == "" && diagnostic != "original runtime failure" {
				t.Fatalf("failure diagnostic replaced: %s", diagnostic)
			}

			if test.noEvents {
				if settled.ReconciliationJobID != "" || settled.SuccessorJobID != "" || settled.State != workflow.StateNeedsHuman {
					t.Fatalf("no events created work: %#v", settled)
				}
				return
			}

			reconciliation, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "association-replay", time.Minute)
			if err != nil || reconciliation == nil || reconciliation.ID != settled.ReconciliationJobID {
				t.Fatalf("claim replay = %#v, %v", reconciliation, err)
			}
			ack, err := database.AcknowledgePendingEventReconciliation(ctx, *reconciliation, settlementPendingEventFactory)
			if err != nil {
				t.Fatal(err)
			}
			again, err := databases[0].AcknowledgePendingEventReconciliation(ctx, *reconciliation, settlementPendingEventFactory)
			// Completed jobs reject a stale lease rather than replaying deliveries.
			if !errors.Is(err, store.ErrPendingEventReconciliationFenceLost) || again.SuccessorJobID != "" {
				t.Fatalf("completed reconciliation accepted stale lease: %#v, %v", again, err)
			}

			if (outcome == workflow.TurnOutcomeBlocked || test.mode == "handoff") && ack.SuccessorJobID != "" {
				t.Fatalf("bookkeeping resumed blocked work: %#v", ack)
			}
			if err := pool.QueryRow(ctx, `SELECT head_sha,COALESCE(ready_for_sha,'') FROM change_proposals WHERE id=$1`, settled.ChangeProposalID).Scan(&head, &ready); err != nil {
				t.Fatal(err)
			}
			var proposals, completedEvents int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM change_proposals WHERE workflow_id=$1),(SELECT count(*) FROM normalized_events WHERE workflow_id=$1 AND status='COMPLETED')`, execution.WorkflowID).Scan(&proposals, &completedEvents); err != nil {
				t.Fatal(err)
			}
			wantEvents := 4
			if test.omitLast {
				wantEvents = 3
			}
			if proposals != 1 || completedEvents != wantEvents {
				t.Fatalf("durability = %d proposals, %d completed events", proposals, completedEvents)
			}
			if outcome == workflow.TurnOutcomeBlocked {
				var reason, diagnostic string
				if err := pool.QueryRow(ctx, `SELECT human_handoff_reason,(SELECT payload->>'diagnostic' FROM jobs WHERE workflow_id=$1 AND kind='PUBLISH_HUMAN_HANDOFF') FROM workflows WHERE id=$1`, execution.WorkflowID).Scan(&reason, &diagnostic); err != nil {
					t.Fatal(err)
				}
				if reason != string(workflow.ReasonAgentBlocked) || diagnostic != "Required fixture unavailable" {
					t.Fatalf("original blocker replaced: %s / %s", reason, diagnostic)
				}
			}
			if outcome == workflow.TurnOutcomeInfrastructureFailed && test.mode != "handoff" && (ack.Successor.ExpectedHeadSHA != heads[2] || ack.Successor.Role != workflow.RoleDeveloper || ack.Successor.Purpose != workflow.TurnPurposeRetry) {
				t.Fatalf("retry not scoped to proven head: %#v", ack)
			}

			if head != heads[2] || ready != "" {
				t.Fatalf("replay rewound or readied proposal: %s / %s", head, ready)
			}
		})
	}
}

func TestPublicationAssociationRequiresSuccessfulMatchingEvidence(t *testing.T) {
	for _, test := range []string{"no open", "failed open", "wrong repository", "wrong branch", "incomplete old result", "mismatching readiness PR", "action persistence conflict"} {
		t.Run(test, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 116, workflow.RoleDeveloper, "")
			execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
			if err != nil {
				t.Fatal(err)
			}
			const base = "0123456789abcdef0123456789abcdef01234567"
			const head = "1123456789abcdef0123456789abcdef01234567"
			api := &replayGatewayGitHub{head: head, branch: "omnigrex/issue-116", base: "main"}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: replayGatewayCredentials{}, Publisher: &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: head, Changed: true}}}, Workflow: mcp.LedgerWorkflowMutations{}})
			if err != nil {
				t.Fatal(err)
			}
			gateway, registration := openReplayGateway(t, database, backend, replayGatewayScope(lease, execution, base))
			callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish"}, false)
			if test != "no open" {
				callReplayGatewayTool(t, gateway, registration, mcp.ToolOpenPR, map[string]any{"operation_id": "open", "title": "Changes", "body": "Not ready"}, false)
			}
			if err := gateway.CloseAndDrain(ctx, registration); err != nil {
				t.Fatal(err)
			}
			observation := successfulSettlementObservation(workflow.TurnOutcomeBlocked, nil)
			switch test {
			case "failed open":
				if _, err := pool.Exec(ctx, `UPDATE tool_invocations SET state='FAILED',result=NULL,last_error='open failed' WHERE agent_turn_id=$1 AND tool_name='open_pr'`, lease.ID); err != nil {
					t.Fatal(err)
				}
			case "wrong repository":
				if _, err := pool.Exec(ctx, `UPDATE tool_invocations SET result=jsonb_set(result,'{repository_id}','999'::jsonb) WHERE agent_turn_id=$1 AND tool_name='open_pr'`, lease.ID); err != nil {
					t.Fatal(err)
				}
			case "wrong branch":
				if _, err := pool.Exec(ctx, `UPDATE tool_invocations SET result=jsonb_set(result,'{head_ref}','"unrelated"'::jsonb) WHERE agent_turn_id=$1 AND tool_name='open_pr'`, lease.ID); err != nil {
					t.Fatal(err)
				}
			case "incomplete old result":
				if _, err := pool.Exec(ctx, `UPDATE tool_invocations SET result=result-'version'-'base_sha' WHERE agent_turn_id=$1 AND tool_name='open_pr'`, lease.ID); err != nil {
					t.Fatal(err)
				}
			case "action persistence conflict":
				observation = failedSettlementObservation("original runtime failure")
				if _, err := pool.Exec(ctx, `INSERT INTO jobs(id,queue,kind,payload,status,max_attempts,idempotency_key,workflow_id,workflow_attempt_id) VALUES('11600000-0000-4000-8000-000000000088','workflow','PREPARE_AGENT_TURN','{}'::jsonb,'AVAILABLE',3,'forced-publication-successor',$1,$2)`, fixture.workflowID, fixture.attemptID); err != nil {
					t.Fatal(err)
				}

			case "mismatching readiness PR":
				observation = successfulSettlementObservation(workflow.TurnOutcomeChangeProposalReady, &store.AgentTurnSettlementChangeProposal{WorkflowID: fixture.workflowID, RepositoryID: 116, RepositoryOwner: "owner", RepositoryName: "repo", PullRequestID: 999, PullRequestNumber: 99, PullRequestNodeID: "PR_unrelated", Status: "OPEN", Active: true, BaseRef: "main", BaseSHA: api.pullRequest().Base.SHA, HeadRef: api.branch, HeadSHA: head})
			}
			if err := database.CloseMutationAdmission(ctx, lease); err != nil {
				t.Fatal(err)
			}
			settled, err := database.SettleAgentTurn(ctx, lease, observation)
			if test == "no open" || test == "failed open" {
				if err != nil || settled.ChangeProposalID != "" || settled.State != workflow.StateNeedsHuman {
					t.Fatalf("nonpublication adopted: %#v, %v", settled, err)
				}
			} else if test == "action persistence conflict" {
				if err == nil {
					t.Fatal("action conflict committed association")
				}
				assertSettlementRolledBack(t, pool, ctx, fixture.workflowID, lease.ID, workflow.StateDeveloping)
			} else if !errors.Is(err, store.ErrAgentTurnPublicationConflict) && !errors.Is(err, store.ErrAgentTurnChangeProposalConflict) {
				t.Fatalf("contradictory evidence error = %v", err)
			}
			var proposals int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM change_proposals WHERE workflow_id=$1`, fixture.workflowID).Scan(&proposals); err != nil {
				t.Fatal(err)
			}
			if proposals != 0 {
				t.Fatalf("adopted %d unproven proposals", proposals)
			}
		})
	}
}

func TestInitialAssociationOpensExactlyBoundRecoveredBranch(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprintf("replay_bound_publish_%t", replay), func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 2)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			fixture := seedAgentSession(t, pool, 116)
			if _, err := pool.Exec(ctx, `UPDATE workflows SET status='DEVELOPING',state_revision=1,desired_assignment_status='ACTIVE',desired_runtime_state='ACTIVE' WHERE id=$1`, fixture.workflowID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit=1,current_stage='implementation' WHERE id=$1`, fixture.attemptID); err != nil {
				t.Fatal(err)
			}
			const base = "0123456789abcdef0123456789abcdef01234567"
			const head = "1123456789abcdef0123456789abcdef01234567"
			root, lease, execution := acquireReplayGatewayTurn(t, database, pool, fixture, store.AgentTurn{}, "branch-only")
			api := &replayGatewayGitHub{head: head, branch: "omnigrex/issue-116", base: "main"}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: replayGatewayCredentials{}, Publisher: &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: head, Changed: true}}}, Workflow: mcp.LedgerWorkflowMutations{}})
			if err != nil {
				t.Fatal(err)
			}
			gateway, registration := openReplayGateway(t, database, backend, replayGatewayScope(lease, execution, base))
			callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish"}, false)
			if err := gateway.CloseAndDrain(ctx, registration); err != nil {
				t.Fatal(err)
			}
			if err := database.CloseMutationAdmission(ctx, lease); err != nil {
				t.Fatal(err)
			}
			ledger, err := database.ListAgentTurnMutationInvocations(ctx, lease)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.FinalizeAgentTurn(ctx, lease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "Interrupted before opening PR"}); err != nil {
				t.Fatal(err)
			}
			_, retryLease, retryExecution := acquireReplayGatewayTurn(t, database, pool, fixture, root, "recovered-branch")
			if err := database.BindAgentTurnPublication(ctx, retryLease, store.AgentTurnPublication{HeadRef: api.branch, HeadSHA: head, BaseRef: "main", SourcePublishMutationID: ledger[0].ID}); err != nil {
				t.Fatal(err)
			}
			scope := replayGatewayScope(retryLease, retryExecution, head)
			scope.RecoveredPublication = true
			gateway, registration = openReplayGateway(t, database, backend, scope)
			if replay {
				callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish"}, false)
			}
			callReplayGatewayTool(t, gateway, registration, mcp.ToolOpenPR, map[string]any{"operation_id": "open", "title": "Changes", "body": "Not ready"}, false)
			if err := gateway.CloseAndDrain(ctx, registration); err != nil {
				t.Fatal(err)
			}
			if err := database.CloseMutationAdmission(ctx, retryLease); err != nil {
				t.Fatal(err)
			}
			settled, err := databases[1].SettleAgentTurn(ctx, retryLease, successfulSettlementObservation(workflow.TurnOutcomeBlocked, nil))
			if err != nil || settled.ChangeProposalID == "" || settled.SuccessorJobID != "" {
				t.Fatalf("bound branch settlement = %#v, %v", settled, err)
			}
			var gotHead string
			if err := pool.QueryRow(ctx, `SELECT head_sha FROM change_proposals WHERE id=$1`, settled.ChangeProposalID).Scan(&gotHead); err != nil {
				t.Fatal(err)
			}
			if gotHead != head {
				t.Fatalf("bound head = %s", gotHead)
			}
		})
	}
}

func TestPendingSynchronizationDistinguishesMissingAssociationFromIdentityMismatch(t *testing.T) {
	for _, withOpen := range []bool{false, true} {
		t.Run(fmt.Sprintf("successful_open_%t", withOpen), func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 116, workflow.RoleDeveloper, "")
			if withOpen {
				execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
				if err != nil {
					t.Fatal(err)
				}
				const base = "0123456789abcdef0123456789abcdef01234567"
				const head = "1123456789abcdef0123456789abcdef01234567"
				api := &replayGatewayGitHub{head: head, branch: "omnigrex/issue-116", base: "main"}
				backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: replayGatewayCredentials{}, Publisher: &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: head, Changed: true}}}, Workflow: mcp.LedgerWorkflowMutations{}})
				if err != nil {
					t.Fatal(err)
				}
				gateway, registration := openReplayGateway(t, database, backend, replayGatewayScope(lease, execution, base))
				callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish"}, false)
				callReplayGatewayTool(t, gateway, registration, mcp.ToolOpenPR, map[string]any{"operation_id": "open", "title": "Changes", "body": "Not ready"}, false)
				if err := gateway.CloseAndDrain(ctx, registration); err != nil {
					t.Fatal(err)
				}
			}
			// The secondary fixture deliberately names a PR other than the successful
			// open. Without a successful open there is no internal identity to compare.
			insertDeferredSettlementNormalizedEvent(t, pool, ctx, fixture.workflowID, lease.ID, deferredSettlementEvent{deliveryID: "11600000-0000-4000-8000-000000000099", eventName: "pull_request", action: "synchronize", beforeSHA: "before", headSHA: "after", createdAt: time.Now()})
			if err := database.CloseMutationAdmission(ctx, lease); err != nil {
				t.Fatal(err)
			}
			settled, err := database.SettleAgentTurn(ctx, lease, successfulSettlementObservation(workflow.TurnOutcomeBlocked, nil))
			if err != nil {
				t.Fatal(err)
			}
			job, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "diagnostic", time.Minute)
			if err != nil || job == nil || job.ID != settled.ReconciliationJobID {
				t.Fatalf("claim = %#v, %v", job, err)
			}
			_, err = database.AcknowledgePendingEventReconciliation(ctx, *job, settlementPendingEventFactory)
			if !errors.Is(err, store.ErrPendingNormalizedEventInvalid) {
				t.Fatalf("identity error = %v", err)
			}
			want := "no internal Change Proposal association"
			if withOpen {
				want = "contradicts the Workflow Change Proposal"
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("diagnostic = %v, want %s", err, want)
			}
		})
	}
}

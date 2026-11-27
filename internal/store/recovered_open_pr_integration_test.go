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

	"github.com/jozala/omnigrex/internal/agentturn"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
	"github.com/jozala/omnigrex/internal/workspace"
)

func TestRecoveredOpenPRBlockedSettlement(t *testing.T) {
	for _, test := range []struct {
		name               string
		publish, crossTurn bool
		invalid            string
		mode               string
	}{
		{name: "corroboration handoff", mode: "corroboration", publish: true}, {name: "corroboration invalid source", mode: "corroboration", invalid: "incomplete"}, {name: "recovery", mode: "recovery", publish: true}, {name: "recovery invalid source", mode: "recovery", publish: true, invalid: "incomplete"}, {name: "no publication"}, {name: "new publication", publish: true}, {name: "cross Turn chain", publish: true, crossTurn: true},
		{name: "incomplete source", invalid: "incomplete"}, {name: "legacy open", invalid: "legacy open"}, {name: "wrong operation", invalid: "operation"}, {name: "cyclic link", crossTurn: true, invalid: "cycle"}, {name: "failed source", invalid: "failed"}, {name: "unresolved source", invalid: "unresolved"}, {name: "foreign source", invalid: "foreign"}, {name: "missing chain link", crossTurn: true, invalid: "missing link"},
	} {
		t.Run(test.name, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 2)
			database := databases[0]
			fixture := seedAgentSession(t, pool, 197)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failure_limit = 1, current_stage='implementation' WHERE id = $1`, fixture.attemptID); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE workflows SET status='DEVELOPING',state_revision=1,desired_assignment_status='ACTIVE',desired_runtime_state='ACTIVE' WHERE id=$1`, fixture.workflowID); err != nil {
				t.Fatal(err)
			}
			const second = "2123456789abcdef0123456789abcdef01234567"
			const third = "3123456789abcdef0123456789abcdef01234567"
			const base = "0123456789abcdef0123456789abcdef01234567"
			const head = "1123456789abcdef0123456789abcdef01234567"
			github := &recoveredPRGitHub{replayGatewayGitHub: &replayGatewayGitHub{head: head, branch: "omnigrex/issue-197", base: "main"}}
			backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{
				GitHub: github, Credentials: replayGatewayCredentials{},
				Publisher: &replayGatewayPublisher{results: []workspace.PublicationResult{{Head: head, Changed: true}, {Head: second, Changed: true}, {Head: third, Changed: true}}},
				Workflow:  mcp.LedgerWorkflowMutations{},
			})
			if err != nil {
				t.Fatal(err)
			}
			root, lease, execution := acquireReplayGatewayTurn(t, database, pool, fixture, store.AgentTurn{}, "publication-root")
			gateway, registration := openReplayGateway(t, database, backend, replayGatewayScope(lease, execution, base))
			callReplayGatewayTool(t, gateway, registration, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1", "message": "Fix"}, false)
			callReplayGatewayTool(t, gateway, registration, mcp.ToolOpenPR, map[string]any{"operation_id": "open-1", "title": "Fix", "body": "Ready"}, false)
			if err := gateway.CloseAndDrain(ctx, registration); err != nil {
				t.Fatal(err)
			}
			if err := database.CloseMutationAdmission(ctx, lease); err != nil {
				t.Fatal(err)
			}
			if err := database.FinalizeAgentTurn(ctx, lease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "prompt ended"}); err != nil {
				t.Fatal(err)
			}
			_, nextLease, nextExecution := acquireReplayGatewayTurn(t, database, pool, fixture, root, "publication-retry")
			remote := &recoveredPRRemote{head: head}
			if err := agentturn.NewPublicationRecoveryWithCredentials(database, remote, github, nil).Recover(ctx, nextLease, nextExecution,
				"https://github.com/owner/repo.git", "developer-token", "main"); err != nil {
				t.Fatal(err)
			}
			bound, err := database.GetAgentTurnExecutionContext(ctx, nextLease)
			if err != nil || bound.Publication == nil || bound.Publication.PullRequestID != 654 || bound.Publication.HeadSHA != head {
				t.Fatalf("bound next Turn = %#v, error %v", bound, err)
			}

			latest := head
			if test.crossTurn {
				scope := replayGatewayScope(nextLease, bound, head)
				scope.PullRequest = &mcp.PullRequestScope{ID: 654, Number: 23}
				scope.BranchExists = true
				scope.RecoveredPublication = true
				g, r := openReplayGateway(t, database, backend, scope)
				callReplayGatewayTool(t, g, r, mcp.ToolPublishChanges, map[string]any{"operation_id": "middle-publish"}, false)
				if err := g.CloseAndDrain(ctx, r); err != nil {
					t.Fatal(err)
				}
				if err := database.CloseMutationAdmission(ctx, nextLease); err != nil {
					t.Fatal(err)
				}
				if err := database.FinalizeAgentTurn(ctx, nextLease, store.AgentTurnCompletion{Status: store.AgentTurnFailed, LastError: "middle interruption"}); err != nil {
					t.Fatal(err)
				}
				_, nextLease, nextExecution = acquireReplayGatewayTurn(t, database, pool, fixture, root, "third-turn")
				github.pr.Head.SHA = second
				remote.head = second
				if err := agentturn.NewPublicationRecoveryWithCredentials(database, remote, github, nil).Recover(ctx, nextLease, nextExecution, "https://github.com/owner/repo.git", "developer-token", "main"); err != nil {
					t.Fatal(err)
				}
				bound, err = database.GetAgentTurnExecutionContext(ctx, nextLease)
				if err != nil {
					t.Fatal(err)
				}
				latest = second
			}
			scope := replayGatewayScope(nextLease, bound, latest)
			scope.PullRequest = &mcp.PullRequestScope{ID: 654, Number: 23}
			scope.BranchExists = true
			scope.RecoveredPublication = true
			g, r := openReplayGateway(t, database, backend, scope)
			// Old operation replay is acknowledged without rewinding the binding.
			callReplayGatewayTool(t, g, r, mcp.ToolPublishChanges, map[string]any{"operation_id": "publish-1", "message": "Fix"}, false)
			if test.publish {
				callReplayGatewayTool(t, g, r, mcp.ToolPublishChanges, map[string]any{"operation_id": "final-publish"}, false)
				latest = second
				if test.crossTurn {
					latest = third
				}
			}
			if test.invalid != "" {
				query := `UPDATE tool_invocations SET result='{}' WHERE id=$1`
				switch test.invalid {
				case "legacy open":
					query = `UPDATE tool_invocations SET result='{}' WHERE id=$1`
					bound.Publication.SourcePublishMutationID = bound.Publication.SourceOpenPRMutationID
				case "operation":
					query = `UPDATE tool_invocations SET request='{"operation_id":"different"}' WHERE id=$1`
				case "cycle":
					query = `UPDATE agent_turn_publications SET source_publish_mutation_id=$1 WHERE agent_turn_id=(SELECT agent_turn_id FROM tool_invocations WHERE id=$1)`
				case "unresolved":
					query = `UPDATE tool_invocations SET state='UNKNOWN',result=NULL WHERE id=$1`
				case "failed":
					query = `UPDATE tool_invocations SET state='FAILED' WHERE id=$1`
				case "foreign":
					query = `UPDATE tool_invocations SET external_resource_id='999:foreign' WHERE id=$1`
				case "missing link":
					query = `DELETE FROM agent_turn_publications WHERE agent_turn_id=(SELECT agent_turn_id FROM tool_invocations WHERE id=$1)`
				}
				if _, err := pool.Exec(ctx, query, bound.Publication.SourcePublishMutationID); err != nil {
					t.Fatal(err)
				}
			}
			// Delivery order is deliberately opposite publication order, with a duplicate.
			if test.invalid == "" {
				edges := [][2]string{}
				if test.crossTurn {
					edges = append(edges, [2]string{head, second})
				}
				if test.publish {
					before := head
					if test.crossTurn {
						before = second
					}
					edges = append(edges, [2]string{before, latest})
				}
				if len(edges) > 0 {
					edges = append(edges, edges[0])
				}
				for i := len(edges) - 1; i >= 0; i-- {
					edge := edges[i]
					delivery := fmt.Sprintf("23100000-0000-4000-8000-%012d", i+1)
					claim := claimWorkflowDelivery(t, database, ctx, store.WebhookDelivery{DeliveryID: delivery, EventName: "pull_request", Action: "synchronize", RepositoryID: bound.Repository.ID, RepositoryOwner: bound.Repository.Owner, RepositoryName: bound.Repository.Name, Headers: map[string]string{}, Payload: []byte(`{}`)})
					payload, err := json.Marshal(map[string]any{"delivery_id": delivery, "event": "pull_request", "action": "synchronize", "repository": map[string]any{"id": bound.Repository.ID, "owner": bound.Repository.Owner, "name": bound.Repository.Name}, "pull_request": map[string]any{"id": 654, "number": 23, "head_sha": edge[1], "before_sha": edge[0], "head_ref": github.branch, "base_ref": "main", "base_sha": github.pullRequest().Base.SHA, "node_id": "PR_654"}})
					if err != nil {
						t.Fatal(err)
					}
					app, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken, payload, store.WorkflowLocator{WorkflowID: bound.WorkflowID, RepositoryID: bound.Repository.ID, PullRequestID: 654, PullRequestNumber: 23}, func(c store.WorkflowEventContext) (workflow.Event, error) {
						return workflow.SynchronizationEvent{EventMetadata: c.Metadata, ChangeProposalID: 654, PreviousHeadSHA: edge[0], HeadSHA: edge[1]}, nil
					})
					if err != nil || app.Status != store.NormalizedEventDeferred {
						t.Fatal(app, err)
					}
				}
			}
			if test.mode == "corroboration" {
				github.pr.Head.SHA = latest
				callReplayGatewayTool(t, g, r, mcp.ToolRequestReview, map[string]any{"operation_id": "ready", "summary": "Ready"}, false)
			} else if test.mode != "recovery" {
				callReplayGatewayTool(t, g, r, mcp.ToolReportBlocked, map[string]any{"operation_id": "blocked", "reason": "Fixture qualification unavailable"}, false)
			}
			if err := g.CloseAndDrain(ctx, r); err != nil {
				t.Fatal(err)
			}

			observation := successfulSettlementObservation(workflow.TurnOutcomeBlocked, nil)
			observation.Diagnostic = "Fixture qualification unavailable"
			var settled store.AgentTurnSettlement
			if test.mode == "corroboration" {
				if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, nextLease, "completed", ""); err != nil {
					t.Fatal(err)
				}
				ledger, err := database.ListAgentTurnMutationInvocations(ctx, nextLease)
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
				if _, err := database.BeginTerminalCorroboration(ctx, nextLease, source, prompt, "", "transient_transport"); err != nil {
					t.Fatal(err)
				}
				verifier, err := databases[1].ClaimJobKind(ctx, "agent-turn-recovery", store.VerifyTerminalIntentJobKind, "recovered-verifier", time.Minute)
				if err != nil || verifier == nil {
					t.Fatal(verifier, err)
				}
				settled, err = databases[1].CompleteTerminalCorroborationHandoff(ctx, *verifier, workflow.ReasonTerminalCorroborationExhausted, "transient_transport")
				if err != nil {
					t.Fatal(err)
				}
				again, err := database.CompleteTerminalCorroborationHandoff(ctx, *verifier, workflow.ReasonTerminalCorroborationExhausted, "transient_transport")
				if err != nil || again != settled {
					t.Fatal(again, err)
				}
			} else if test.mode == "recovery" {
				// A prior interrupted execution has already spent the retry.
				// Association must not change this exhausted recovery policy.
				if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET infrastructure_failures=1 WHERE id=$1`, fixture.attemptID); err != nil {
					t.Fatal(err)
				}
				if err := database.CloseMutationAdmission(ctx, nextLease); err != nil {
					t.Fatal(err)
				}
				for _, q := range []struct {
					sql  string
					args []any
				}{
					{`UPDATE jobs SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, []any{nextLease.JobLease.ID}},
					{`UPDATE job_attempts SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE job_id=$1 AND attempt_number=$2`, []any{nextLease.JobLease.ID, nextLease.JobLease.Attempt}},
					{`UPDATE agent_turns SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, []any{nextLease.ID}},
					{`UPDATE agent_turn_slots SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE agent_turn_id=$1`, []any{nextLease.ID}},
				} {
					if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := databases[1].RecoverExpiredAgentTurn(ctx, nextLease.ID, nextLease.ExecutionEpoch); err != nil {
					t.Fatal(err)
				}
				mutations := claimRecoveryJob(t, databases[1], ctx, store.ReconcileAgentTurnMutationsJobKind, "recovered-mutations")
				stop := claimRecoveryJob(t, databases[1], ctx, store.StopStaleRuntimeJobKind, "recovered-stop")
				if _, err := databases[1].AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
					t.Fatal(err)
				}
				completed, err := databases[1].CompleteAgentTurnMutationReconciliation(ctx, mutations)
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
				replay, err := database.CompleteAgentTurnRecovery(ctx, nextLease.ID, nextLease.ExecutionEpoch)
				if err != nil || replay.SettlementID != settled.ID {
					t.Fatal(replay, err)
				}
			} else {
				if err := database.CloseMutationAdmission(ctx, nextLease); err != nil {
					t.Fatal(err)
				}
				settled, err = databases[1].SettleAgentTurn(ctx, nextLease, observation)
				if err != nil {
					t.Fatal(err)
				}
				again, err := database.SettleAgentTurn(ctx, nextLease, observation)
				if err != nil || again != settled {
					t.Fatal(again, err)
				}
			}
			if settled.State != workflow.StateNeedsHuman || settled.SuccessorJobID != "" {
				t.Fatal(settled)
			}

			if github.openCallCount() != 1 {
				t.Fatal("duplicate open", github.openCallCount())
			}
			if test.invalid != "" {
				if settled.ChangeProposalID != "" || settled.PublicationAssociationDiagnostic == "" {
					t.Fatal("invalid evidence granted association or lost diagnostic", settled)
				}
			} else {
				if settled.ChangeProposalID == "" || settled.PublicationAssociationDiagnostic != "" {
					t.Fatal("lost valid association", settled)
				}
				if settled.ReconciliationJobID != "" {
					job, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.ReconcilePendingEventsJobKind, "recovered-reconcile", time.Minute)
					if err != nil || job == nil {
						t.Fatal(job, err)
					}
					ack, err := database.AcknowledgePendingEventReconciliation(ctx, *job, settlementPendingEventFactory)
					if err != nil || ack.SuccessorJobID != "" {
						t.Fatal(ack, err)
					}
					_, err = databases[1].AcknowledgePendingEventReconciliation(ctx, *job, settlementPendingEventFactory)
					if !errors.Is(err, store.ErrPendingEventReconciliationFenceLost) {
						t.Fatal(err)
					}
				}
				var got, ready, turnProposal, turnHead string
				if err := pool.QueryRow(ctx, `SELECT p.head_sha,COALESCE(p.ready_for_sha,''),COALESCE(t.change_proposal_id::text,''),COALESCE(t.expected_head_sha,'') FROM change_proposals p JOIN agent_turns t ON t.id=$2 WHERE p.id=$1`, settled.ChangeProposalID, nextLease.ID).Scan(&got, &ready, &turnProposal, &turnHead); err != nil {
					t.Fatal(err)
				}
				if got != latest || ready != "" || turnProposal != "" || turnHead != nextLease.ExpectedHeadSHA {
					t.Fatal(got, ready, turnProposal, turnHead)
				}
			}
			var diagnostic string
			var used int
			if err := pool.QueryRow(ctx, `SELECT a.infrastructure_failures,(SELECT payload->>'diagnostic' FROM jobs WHERE workflow_id=$1 AND kind='PUBLISH_HUMAN_HANDOFF') FROM workflow_attempts a WHERE a.id=$2`, fixture.workflowID, fixture.attemptID).Scan(&used, &diagnostic); err != nil {
				t.Fatal(err)
			}
			wantUsed := 0
			wantDiagnostic := "Fixture qualification unavailable"
			if test.mode == "recovery" {
				wantUsed = 1
				wantDiagnostic = "Agent Turn execution was interrupted during recovery"
			}
			if test.mode == "corroboration" {
				wantDiagnostic = "Terminal mutation succeeded but fresh corroboration was unavailable: transient_transport"
			}
			diagnosticMatches := diagnostic == wantDiagnostic
			if test.mode == "recovery" {
				diagnosticMatches = strings.HasPrefix(diagnostic, wantDiagnostic+" [agent_turn_id=")
			}
			if used != wantUsed || !diagnosticMatches {
				t.Fatal("blocker or budget replaced", used, diagnostic)
			}
		})
	}
}

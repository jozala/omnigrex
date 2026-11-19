//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/collaboration"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/mcp"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

type requestRejectionGitHub struct {
	replayGatewayGitHub
	posts int
}

func (g *requestRejectionGitHub) CreateIssueComment(_ context.Context, _, _, _ string, _ int, r githubapi.CommentRequest) (githubapi.IssueComment, error) {
	g.posts++
	return githubapi.IssueComment{ID: int64(g.posts), NodeID: "IC_fixture", Body: githubapi.JoinBodyParts(r.Body, r.Marker), HTMLURL: "https://github.test/comment"}, nil
}

func TestRequestRejectionGatewayConversationAndCorrection(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	number := 9970
	for _, r := range []workflow.Role{workflow.RolePlanner, workflow.RoleDeveloper, workflow.RoleReviewer} {
		for _, replacement := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/replacement=%t", r, replacement), func(t *testing.T) {
				number++
				database := databases[0]
				fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, number, r, strings.Repeat("a", 40))
				var state collaboration.State
				original := []collaboration.Question{{ID: "Q1", Body: "Use a received-input index?"}, {ID: "Q2", Body: "Keep substantive status comments?"}}
				if _, err := state.Request("R1", lease.AgentParticipantID, lease.ID, "head", original); err != nil {
					t.Fatal(err)
				}
				if err := state.Publish("R1", collaboration.Source{Channel: "fixture", ID: "r1"}); err != nil {
					t.Fatal(err)
				}
				receipt := state.Accept("A1", collaboration.Command{Kind: "answer", Target: "R1", Body: "Explain Q1 with simple examples. Q2: yes."}, collaboration.Admission{Active: true, Authorized: true, ParticipantID: lease.AgentParticipantID, Actor: collaboration.Actor{ID: "human"}, Source: collaboration.Source{Channel: "fixture", ID: "a1"}, At: time.Now()})
				if receipt.ResponseID != "A1" {
					t.Fatal(receipt)
				}
				state.Prepare(lease.AgentParticipantID, lease.ID)
				raw, _ := json.Marshal(state)
				if _, err := pool.Exec(ctx, `INSERT INTO work_item_collaboration(workflow_id,state) VALUES($1,$2)`, fixture.workflowID, raw); err != nil {
					t.Fatal(err)
				}
				execution, err := database.GetAgentTurnExecutionContext(ctx, lease)
				if err != nil {
					t.Fatal(err)
				}
				api := &requestRejectionGitHub{}
				backend, err := mcp.NewProductionBackend(mcp.ProductionBackendConfig{GitHub: api, Credentials: replayGatewayCredentials{}, Publisher: &replayGatewayPublisher{}, Workflow: mcp.LedgerWorkflowMutations{}, Clarifications: database})
				if err != nil {
					t.Fatal(err)
				}
				scope := replayGatewayScope(lease, execution, strings.Repeat("a", 40))
				scope.Role = r
				scope.PullRequest = &mcp.PullRequestScope{ID: proposal.PullRequestID, Number: proposal.PullRequestNumber}
				scope.Branch = proposal.HeadRef
				gateway, registration := openReplayGateway(t, database, backend, scope)
				call := func(tool string, args map[string]any) string {
					return callReplayGatewayTool(t, gateway, registration, tool, args, false)
				}
				call(mcp.ToolAssessHumanInput, map[string]any{"operation_id": "assess", "assessments": []map[string]any{{"input_id": "A1", "evidence_ids": []string{"A1"}, "disposition": "changed", "explanation": "Q1 needs examples; Q2 accepted."}}})
				call(mcp.ToolResolveClarification, map[string]any{"operation_id": "resolve-q2", "round_id": "R1", "resolutions": []collaboration.Resolution{{QuestionID: "Q2", ResponseIDs: []string{"A1"}, Explanation: "Keep substantive status comments."}}})
				request := func(op string, questions []collaboration.Question) map[string]any {
					return map[string]any{"operation_id": op, "introduction": "One updated directory comment can list A2 so people can find an answer.", "questions": questions}
				}
				bad := request("changed-body", []collaboration.Question{{ID: "Q1", Body: "Use one updated directory comment?"}})
				first := call(mcp.ToolRequestClarification, bad)
				if !strings.Contains(first, "question_identity_content_changed") || strings.Contains(first, "CLARIFICATION_WAIT") || api.posts != 0 {
					t.Fatal(first, api.posts)
				}
				if replay := call(mcp.ToolRequestClarification, bad); !reflect.DeepEqual(requestResult(t, replay), requestResult(t, first)) {
					t.Fatal("replay changed", replay, first)
				}
				count := 51
				if r == workflow.RolePlanner {
					count = 6
				}
				many := make([]collaboration.Question, count)
				for i := range many {
					many[i] = collaboration.Question{ID: fmt.Sprint("Q", i), Body: "Private source body"}
				}
				for _, tc := range []struct {
					op, reason string
					qs         []collaboration.Question
				}{
					{"duplicate", "duplicate_question", []collaboration.Question{original[0], original[0]}},
					{"drop", "unresolved_question_dropped", []collaboration.Question{original[1]}},
					{"empty", "empty_questions_without_wait", []collaboration.Question{}},
					{"count", "question_limit_exceeded", many},
				} {
					result := call(mcp.ToolRequestClarification, request(tc.op, tc.qs))
					if !strings.Contains(result, tc.reason) || strings.Contains(result, "CLARIFICATION_WAIT") || strings.Contains(result, "Private source body") {
						t.Fatal(result)
					}
					if tc.op == "count" {
						replay := call(mcp.ToolRequestClarification, request(tc.op, tc.qs))
						if !reflect.DeepEqual(requestResult(t, result), requestResult(t, replay)) {
							t.Fatal(result, replay)
						}
						var allocated int
						if err = pool.QueryRow(ctx, `SELECT count(*) FROM tool_invocations WHERE agent_turn_id=$1 AND operation_id='count'`, lease.ID).Scan(&allocated); err != nil || allocated != 0 {
							t.Fatal(allocated, err)
						}
					}
				}
				if err = pool.QueryRow(ctx, `SELECT state FROM work_item_collaboration WHERE workflow_id=$1`, fixture.workflowID).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				if err = json.Unmarshal(raw, &state); err != nil {
					t.Fatal(err)
				}
				if len(state.Rounds) != 1 || len(state.References) != 2 || len(state.Assessments) != 1 || len(state.Rounds[0].Resolutions) != 1 || len(state.RequestRejections) != 4 || api.posts != 0 || state.Waiting || state.Rounds[0].Artifact != nil || state.Rounds[0].Questions[0] != original[0] {
					t.Fatal("rejection changed conversation", state, api.posts)
				}
				corrected := []collaboration.Question{original[0]}
				if replacement {
					corrected = []collaboration.Question{{ID: "Q1-v2", Replaces: "Q1", Body: "Use one updated directory comment?"}}
				}
				good := call(mcp.ToolRequestClarification, request("corrected", corrected))
				if !strings.Contains(good, "CLARIFICATION_WAIT") || !strings.Contains(good, `\"reference\":\"R2\"`) || api.posts != 1 {
					t.Fatal(good, api.posts)
				}
				call(mcp.ToolRequestClarification, request("corrected", corrected))
				if replay := call(mcp.ToolRequestClarification, bad); !reflect.DeepEqual(requestResult(t, replay), requestResult(t, first)) || api.posts != 1 {
					t.Fatal("old rejection changed after publication", replay, api.posts)
				}
				callReplayGatewayTool(t, gateway, registration, mcp.ToolRequestClarification, request("changed-body", corrected), true)
				if err = gateway.CloseAndDrain(ctx, registration); err != nil {
					t.Fatal(err)
				}
				if err = database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
					t.Fatal(err)
				}
				// Four successful handled rejections do not compete for the terminal source.
				var source string
				if err = pool.QueryRow(ctx, `SELECT id::text FROM tool_invocations WHERE agent_turn_id=$1 AND operation_id='corrected'`, lease.ID).Scan(&source); err != nil {
					t.Fatal(err)
				}
				if _, err = database.BeginTerminalCorroboration(ctx, lease, source, json.RawMessage(`{"admitted":true,"status":"completed"}`), "", "transient_transport"); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func requestResult(t *testing.T, response string) any {
	t.Helper()
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(response), &envelope); err != nil || len(envelope.Result.Content) != 1 {
		t.Fatal(response, err)
	}
	return requestJSON(t, []byte(envelope.Result.Content[0].Text))
}
func requestJSON(t *testing.T, raw []byte) any {
	t.Helper()
	var result any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func startQuestionRequest(t *testing.T, database *store.Store, ctx context.Context, lease store.AgentTurnLease, operation string, qs []collaboration.Question) store.MutationReservation {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"operation_id": operation, "questions": qs})
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{OperationID: operation, ToolName: mcp.ToolRequestClarification, Request: raw, ExternalService: "omnigrex", ExternalResourceID: lease.JobLease.WorkflowID})
	if err != nil {
		t.Fatal(err)
	}
	mutation, err = database.StartMutation(ctx, lease, mutation.ID)
	if err != nil {
		t.Fatal(err)
	}
	return mutation
}

func TestRequestRejectionLostAcknowledgmentAndEarlierUnsettledPublication(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	database := databases[0]
	fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 9980, workflow.RoleDeveloper, strings.Repeat("a", 40))
	qs := []collaboration.Question{{ID: "Q1", Body: "Which policy?"}}
	earlier := startQuestionRequest(t, database, ctx, lease, "earlier", qs)
	round, err := database.RequestClarification(ctx, lease, earlier.ID, qs)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.SaveCollaborationArtifact(ctx, lease, round.ID, collaboration.RenderedPublication{Body: "frozen exact payload", Marker: "fixture-marker"}); err != nil {
		t.Fatal(err)
	}
	blocked := startQuestionRequest(t, database, ctx, lease, "blocked", qs)
	_, err = database.RequestClarification(ctx, lease, blocked.ID, qs)
	var validation *collaboration.RequestValidationError
	if !errors.As(err, &validation) || validation.Code != "earlier_publication_unsettled" {
		t.Fatal(err)
	}
	result, err := collaboration.RequestRejectionResult(validation)
	if err != nil || !strings.Contains(string(result), "reconcile_prior_operation") || strings.Contains(string(result), "correct_arguments_new_operation") {
		t.Fatal(string(result), err)
	}
	// Fresh identities cannot bypass the earlier unsettled publication.
	fresh := startQuestionRequest(t, database, ctx, lease, "fresh-bypass", qs)
	if _, err = database.RequestClarification(ctx, lease, fresh.ID, qs); !errors.As(err, &validation) || validation.Code != "earlier_publication_unsettled" {
		t.Fatal(err)
	}
	if err = database.CompleteMutation(ctx, lease, fresh.ID, result); err != nil {
		t.Fatal(err)
	}
	if err = database.MarkMutationUnknown(ctx, lease, blocked.ID, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	receipt, err := databases[1].GetRequestRejection(ctx, fixture.workflowID, lease.AgentParticipantID, lease.ID, lease.ExecutionEpoch, blocked.ID)
	if err != nil || receipt == nil || *receipt != *validation {
		t.Fatal(receipt, err)
	}
	if other, err := database.GetRequestRejection(ctx, fixture.workflowID, lease.AgentParticipantID, lease.ID, lease.ExecutionEpoch+1, blocked.ID); err != nil || other != nil {
		t.Fatal(other, err)
	}
	if _, err = database.RequestClarification(ctx, lease, blocked.ID, qs); err == nil {
		t.Fatal("unknown request reapplied")
	}
	if err = database.ConfirmClarificationPublication(ctx, fixture.workflowID, round.ID, collaboration.Source{Channel: "fixture", ID: "published"}); err != nil {
		t.Fatal(err)
	}
	if err = database.CompleteMutation(ctx, lease, earlier.ID, json.RawMessage(fmt.Sprintf(`{"outcome":"CLARIFICATION_WAIT","round_id":%q}`, round.ID))); err != nil {
		t.Fatal(err)
	}
	expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
	if _, err = database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "request-rejection-stop")
	if _, err = database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	reconciler, err := mcp.NewProductionReconciler(mcp.ProductionReconcilerConfig{GitHub: &replayGatewayGitHub{}, Credentials: replayGatewayCredentials{}, Publications: unusedHandoffPublications{}, Clarifications: databases[1]})
	if err != nil {
		t.Fatal(err)
	}
	recoveryJob := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "request-rejection-recover")
	recovery, err := database.GetAgentTurnMutationReconciliationContext(ctx, recoveryJob)
	if err != nil {
		t.Fatal(err)
	}
	blocked.State = store.MutationUnknown
	recovered, err := reconciler.Reconcile(ctx, recovery, blocked)
	if err != nil || recovered.Disposition != mcp.ReconciliationFound || !reflect.DeepEqual(requestJSON(t, result), requestJSON(t, recovered.Outcome.Result)) {
		t.Fatal(recovered, err)
	}
	// Absent receipt alone cannot fabricate rejection. Existing stopped-and-fenced
	// non-publication proof can still establish failure, not successful waiting.
	if _, err = pool.Exec(ctx, `UPDATE work_item_collaboration SET state=state-'request_rejections' WHERE workflow_id=$1`, fixture.workflowID); err != nil {
		t.Fatal(err)
	}
	recovered, err = reconciler.Reconcile(ctx, recovery, blocked)
	if err != nil || recovered.Disposition != mcp.ReconciliationDefinitelyFailed || recovered.Outcome.State != store.MutationFailed || collaboration.IsRequestRejection(recovered.Outcome.Result) {
		t.Fatal(recovered, err)
	}
}

func TestRequestRejectionAloneCannotEstablishWaiting(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx := t.Context()
	fixture, lease, _ := prepareOpenSettlementTurn(t, database, pool, ctx, 9981, workflow.RoleDeveloper, strings.Repeat("a", 40))
	qs := []collaboration.Question{{ID: "duplicate", Body: "First?"}, {ID: "duplicate", Body: "Second?"}}
	bad := startQuestionRequest(t, database, ctx, lease, "only-rejection", qs)
	_, err := database.RequestClarification(ctx, lease, bad.ID, qs)
	var validation *collaboration.RequestValidationError
	if !errors.As(err, &validation) {
		t.Fatal(err)
	}
	result, err := collaboration.RequestRejectionResult(validation)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.CompleteMutation(ctx, lease, bad.ID, result); err != nil {
		t.Fatal(err)
	}
	if err = database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = database.BeginTerminalCorroboration(ctx, lease, bad.ID, json.RawMessage(`{"admitted":true,"status":"completed"}`), "", "transient_transport"); !errors.Is(err, store.ErrTerminalCorroborationConflict) {
		t.Fatal("rejection consumed terminal source", err)
	}
	expireAgentTurnExecution(t, pool, ctx, lease.JobLease.ID, lease.ID)
	if _, err = database.RecoverExpiredAgentTurn(ctx, lease.ID, lease.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	stop := claimRecoveryJob(t, database, ctx, store.StopStaleRuntimeJobKind, "only-rejection-stop")
	if _, err = database.AcknowledgeRecoveredRuntimeStopped(ctx, stop); err != nil {
		t.Fatal(err)
	}
	mutations := claimRecoveryJob(t, database, ctx, store.ReconcileAgentTurnMutationsJobKind, "only-rejection-reconcile")
	candidates, err := database.ListAgentTurnMutationsForReconciliation(ctx, mutations)
	if err != nil || len(candidates) != 0 {
		t.Fatal("rejection treated as publication effect", candidates, err)
	}
	if _, err = database.CompleteAgentTurnMutationReconciliation(ctx, mutations); err != nil {
		t.Fatal(err)
	}
	recovery, err := database.CompleteAgentTurnRecovery(ctx, lease.ID, lease.ExecutionEpoch)
	if err != nil || recovery.Status == store.AgentTurnCorroborating {
		t.Fatal(recovery, err)
	}
	var waiting bool
	var rounds int
	if err = pool.QueryRow(ctx, `SELECT clarification_waiting,(SELECT count(*) FROM agent_turn_corroborations WHERE workflow_id=$1) FROM workflows WHERE id=$1`, fixture.workflowID).Scan(&waiting, &rounds); err != nil || waiting || rounds != 0 {
		t.Fatal(waiting, rounds, err)
	}
}

//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestCorroborationContextConflictCanCompleteDeterministicHandoff(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	fixture, lease, proposal := prepareOpenSettlementTurn(t, database, pool, ctx, 2501, workflow.RoleDeveloper, "")
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "conflicting-checkpoint", ToolName: "request_review",
		Request:         json.RawMessage(`{"operation_id":"conflicting-checkpoint","summary":"Ready"}`),
		ExternalService: "omnigrex", ExternalResourceID: fmt.Sprintf("%d:%s", proposal.RepositoryID, proposal.HeadRef),
		ExpectedSHA: proposal.HeadSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(fmt.Sprintf(
		`{"outcome":"REVIEW_REQUESTED","pull_request_id":%d,"pull_request_number":%d,"head_sha":%q}`,
		proposal.PullRequestID, proposal.PullRequestNumber, proposal.HeadSHA))); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmissionWithPromptEvidence(ctx, lease, "completed", ""); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := database.BeginTerminalCorroboration(ctx, lease, mutation.ID,
		json.RawMessage(`{"admitted":true,"status":"completed"}`), "", "transient_transport")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := database.ClaimJobKind(ctx, store.AgentTurnRecoveryQueue, store.VerifyTerminalIntentJobKind,
		"verify-conflicting-checkpoint", time.Minute)
	if err != nil || verifier == nil || verifier.ID != checkpoint.VerificationJobID {
		t.Fatalf("claim verifier = (%#v, %v)", verifier, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_turn_corroborations
SET prompt_outcome = '{"admitted":true,"status":"completed","limit_reached":true}'::jsonb
WHERE agent_turn_id = $1`, lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.GetTerminalCorroborationContext(ctx, *verifier); !errors.Is(err, store.ErrTerminalCorroborationConflict) {
		t.Fatalf("invalid checkpoint context = %v", err)
	}
	settled, err := database.CompleteTerminalCorroborationHandoff(ctx, *verifier,
		workflow.ReasonTerminalCorroborationPrerequisite, "terminal_evidence_conflict")
	if err != nil || settled.State != workflow.StateNeedsHuman || settled.Reason != workflow.ReasonTerminalCorroborationPrerequisite || settled.SuccessorJobID != "" {
		t.Fatalf("conflicting checkpoint handoff = (%#v, %v)", settled, err)
	}
	if replayed, err := database.CompleteTerminalCorroborationHandoff(ctx, *verifier,
		workflow.ReasonTerminalCorroborationPrerequisite, "terminal_evidence_conflict"); err != nil || replayed.ID != settled.ID {
		t.Fatalf("idempotent conflict handoff = (%#v, %v)", replayed, err)
	}
	var checkpointState, jobStatus string
	var failures int
	if err := pool.QueryRow(ctx, `SELECT checkpoint.state, job.status, attempt.infrastructure_failures
FROM agent_turn_corroborations AS checkpoint
JOIN jobs AS job ON job.id = checkpoint.verification_job_id
JOIN workflow_attempts AS attempt ON attempt.id = checkpoint.workflow_attempt_id
WHERE checkpoint.agent_turn_id = $1 AND attempt.id = $2`, lease.ID, fixture.attemptID).Scan(&checkpointState, &jobStatus, &failures); err != nil {
		t.Fatal(err)
	}
	if checkpointState != "HANDED_OFF" || jobStatus != "SUCCEEDED" || failures != 0 {
		t.Fatalf("conflict handoff durability = checkpoint %s, verifier %s, infrastructure failures %d", checkpointState, jobStatus, failures)
	}
}

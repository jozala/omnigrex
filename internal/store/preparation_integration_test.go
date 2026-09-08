//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/agentprofile"
	"github.com/jozala/omnigrex/internal/agentturn"
	githubapi "github.com/jozala/omnigrex/internal/github"
	"github.com/jozala/omnigrex/internal/runtime/acp"
	"github.com/jozala/omnigrex/internal/runtime/agentevent"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/runtime/session"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestPrepareAgentTurnAllocatesCreatingSessionBeforeFencedACPBind(t *testing.T) {
	databases, _ := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000006", "62000000-0000-4000-8000-000000000006")
	preparationJob := claimPreparationJob(t, database, ctx)
	if err := database.CompleteJob(ctx, preparationJob, json.RawMessage(`{"unsafe":true}`)); !errors.Is(err, store.ErrWorkflowJobRequiresAcknowledgement) {
		t.Fatalf("CompleteJob() for preparation job error = %v, want fenced acknowledgement", err)
	}
	spec := preparationSpec("atomic-profile", "openai/gpt-5.2")
	spec.Developer.Profile.Config = json.RawMessage(`{
  "steps": 40,
  "runtime": "opencode-acp/1",
  "role": "DEVELOPER",
  "permissions": {"read": "allow", "edit": "deny"},
  "path": ".omnigrex/team/developer.md",
  "name": "developer",
  "model": "openai/gpt-5.2",
  "instructions": "Perform development."
}`)
	wantProfileConfig := agentProfileConfig("developer", workflow.RoleDeveloper, "opencode-acp/1", "openai/gpt-5.2", "", 40, "Perform development.", nil)
	spec.Reviewer.Profile = store.AgentProfileSnapshot{}
	prepared, err := database.PrepareAgentTurn(ctx, preparationJob, spec)
	if err != nil {
		t.Fatalf("PrepareAgentTurn() with unselected empty Reviewer profile error = %v", err)
	}
	if prepared.Assignment.WorkflowID != application.WorkflowID || prepared.Session.AgentAssignmentID != prepared.Assignment.ID ||
		prepared.Session.Status != store.AgentSessionCreating || prepared.Session.ACPSessionID != "" ||
		prepared.Assignment.RuntimeProfileContentSHA256 != spec.Developer.Binding.RuntimeProfileContentSHA256 ||
		prepared.Session.RuntimeProfileContentSHA256 != spec.Developer.Binding.RuntimeProfileContentSHA256 ||
		prepared.Turn.AgentSessionID != prepared.Session.ID || prepared.Turn.ExecutionEpoch != 1 ||
		prepared.Turn.Purpose != workflow.TurnPurposeInitialDevelopment ||
		prepared.Turn.AgentProfileCommitSHA != "atomic-profile" ||
		string(prepared.Turn.AgentProfileConfig) != string(wantProfileConfig) ||
		prepared.Job.Kind != store.RunAgentTurnJobKind || prepared.Job.AgentTurnID != prepared.Turn.ID {
		t.Fatalf("PrepareAgentTurn() = %#v", prepared)
	}
	completedPreparation, err := database.GetJob(ctx, preparationJob.ID)
	if err != nil || completedPreparation.Status != store.JobSucceeded {
		t.Fatalf("preparation Job = (%#v, %v), want SUCCEEDED", completedPreparation, err)
	}
	assignments, err := database.ListAgentAssignments(ctx, application.WorkflowID)
	if err != nil || len(assignments) != 2 {
		t.Fatalf("ListAgentAssignments() = (%#v, %v), want both Roles", assignments, err)
	}
	var reviewer store.AgentAssignment
	for _, assignment := range assignments {
		if assignment.Role == workflow.RoleReviewer {
			reviewer = assignment
		}
	}
	if reviewer.ID == "" || reviewer.RuntimeStatePath == prepared.Assignment.RuntimeStatePath {
		t.Fatalf("Assignment state paths are not isolated: %#v", assignments)
	}
	if reviewer.RuntimeProfileContentSHA256 != spec.Reviewer.Binding.RuntimeProfileContentSHA256 {
		t.Fatalf("Reviewer Assignment Runtime Profile hash = %q, want %q", reviewer.RuntimeProfileContentSHA256, spec.Reviewer.Binding.RuntimeProfileContentSHA256)
	}
	storedSession, err := database.GetAgentSession(ctx, prepared.Session.ID)
	if err != nil || storedSession.RuntimeProfileContentSHA256 != spec.Developer.Binding.RuntimeProfileContentSHA256 {
		t.Fatalf("GetAgentSession() = (%#v, %v), want persisted Runtime Profile hash", storedSession, err)
	}
	if _, err := database.GetAgentSessionForAssignment(ctx, reviewer.ID); !errors.Is(err, store.ErrAgentSessionNotFound) {
		t.Fatalf("Reviewer Session before its first turn error = %v, want ErrAgentSessionNotFound", err)
	}

	runJob := claimAgentTurnJob(t, database, ctx, prepared.Turn, time.Second)
	turnLease, err := database.AcquireAgentTurn(ctx, runJob, prepared.Turn.ControlRevision, "runtime", time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() for CREATING Session error = %v", err)
	}
	labels := store.RuntimeLabels(turnLease.AgentTurn)
	if labels[store.RuntimeLabelAssignmentID] != prepared.Assignment.ID || labels[store.RuntimeLabelSessionID] != prepared.Session.ID ||
		labels[store.RuntimeLabelTurnID] != prepared.Turn.ID || labels[store.RuntimeLabelEpoch] != "1" {
		t.Fatalf("RuntimeLabels() after acquire = %#v", labels)
	}
	bound, err := database.BindAgentSessionACP(ctx, turnLease, "opaque-acp-session", json.RawMessage(`{"z":true,"resume":true}`))
	if err != nil {
		t.Fatalf("BindAgentSessionACP() error = %v", err)
	}
	if bound.Status != store.AgentSessionActive || bound.ACPSessionID != "opaque-acp-session" || string(bound.Capabilities) != `{"resume":true,"z":true}` {
		t.Fatalf("bound Session = %#v", bound)
	}
	stale := turnLease
	stale.OwnerToken = "65000000-0000-4000-8000-000000000006"
	if _, err := database.BindAgentSessionACP(ctx, stale, "opaque-acp-session", bound.Capabilities); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("BindAgentSessionACP() with stale owner error = %v, want ErrAgentTurnFenceLost", err)
	}
	if _, err := database.BindAgentSessionACP(ctx, turnLease, "different-acp-session", bound.Capabilities); !errors.Is(err, store.ErrAgentSessionACPConflict) {
		t.Fatalf("BindAgentSessionACP() conflicting identity error = %v, want ErrAgentSessionACPConflict", err)
	}
}

func TestPrepareAgentTurnSelectsReviewerProfileFromFencedJobIntent(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000010", "62000000-0000-4000-8000-000000000010")
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'REVIEWING' WHERE id = $1`, application.WorkflowID); err != nil {
		t.Fatal(err)
	}
	proposalID := "63000000-0000-4000-8000-000000000010"
	if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha
)
VALUES ($1, $2, 9123, 'owner', 'repo', 10, 10, 'OPEN', 'main', 'base', 'feature', 'review-head')`,
		proposalID, application.WorkflowID); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"mode": workflow.AssignmentGenerationNew, "role": workflow.RoleReviewer,
		"purpose": workflow.TurnPurposeReview, "expected_head_sha": "review-head",
		"retry_of_turn_id": "", "revision": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET payload = $2 WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'`, application.WorkflowID, payload); err != nil {
		t.Fatal(err)
	}

	lease := claimPreparationJob(t, database, ctx)
	prepared, err := database.PrepareAgentTurn(ctx, lease, preparationSpec("profile-set", "openai/developer"))
	if err != nil {
		t.Fatalf("PrepareAgentTurn() Reviewer error = %v", err)
	}
	if prepared.Assignment.Role != workflow.RoleReviewer || prepared.Assignment.AgentProfileName != "reviewer" ||
		prepared.Turn.AgentProfileCommitSHA != "profile-set-reviewer" ||
		!json.Valid(prepared.Turn.AgentProfileConfig) {
		t.Fatalf("Reviewer preparation selected wrong Role profile: %#v", prepared)
	}
	var profile map[string]any
	if err := json.Unmarshal(prepared.Turn.AgentProfileConfig, &profile); err != nil ||
		profile["path"] != ".omnigrex/team/reviewer.md" || profile["model"] != "anthropic/reviewer" {
		t.Fatalf("persisted Reviewer profile = (%#v, %v)", profile, err)
	}
}

func TestPrepareAgentTurnBlocksReviewerWhileDeveloperRecoveryIsUnsettled(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000042", "62000000-0000-4000-8000-000000000042")
	developer := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "recovery-profile", "openai/developer")
	lease := acquireAndBindTurn(t, database, ctx, developer, "developer-recovery-session")
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "developer-recovery-before-review", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.MarkMutationUnknown(ctx, lease, mutation.ID, errors.New("unknown")); err != nil {
		t.Fatal(err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginAgentTurnMutationRecovery(ctx, lease); err != nil {
		t.Fatal(err)
	}

	proposalID := "63000000-0000-4000-8000-000000000042"
	if _, err := pool.Exec(ctx, `UPDATE workflows SET status = 'REVIEWING', state_revision = 2 WHERE id = $1`, application.WorkflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha
)
VALUES ($2, $1, 9123, 'owner', 'repo', 42, 42, 'OPEN', 'main', 'base', 'feature', 'review-head')`,
		application.WorkflowID, proposalID); err != nil {
		t.Fatal(err)
	}
	insertPreparationJob(t, pool, application.WorkflowID, developer.Turn.WorkflowAttemptID, 2,
		workflow.AssignmentGenerationCurrent, workflow.RoleReviewer, workflow.TurnPurposeReview, "review-head", "")
	reviewerJob := claimPreparationJob(t, database, ctx)
	if _, err := database.PrepareAgentTurn(ctx, reviewerJob, preparationSpec("reviewer-after-recovery", "openai/developer")); !errors.Is(err, store.ErrAgentTurnRecoveryUnsettled) {
		t.Fatalf("PrepareAgentTurn() Reviewer successor error = %v, want ErrAgentTurnRecoveryUnsettled", err)
	}
	var reviewerSessions, reviewerTurns int
	var preparationStatus string
	if err := pool.QueryRow(ctx, `
SELECT count(session.id),
       (SELECT count(*) FROM agent_turns WHERE workflow_id = $1 AND id <> $2)
FROM agent_assignments AS assignment
LEFT JOIN agent_sessions AS session ON session.agent_assignment_id = assignment.id
WHERE assignment.workflow_id = $1 AND assignment.role = 'REVIEWER'`,
		application.WorkflowID, developer.Turn.ID).Scan(&reviewerSessions, &reviewerTurns); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM jobs WHERE id = $1`, reviewerJob.ID).Scan(&preparationStatus); err != nil {
		t.Fatal(err)
	}
	if reviewerSessions != 0 || reviewerTurns != 0 || preparationStatus != string(store.JobLeased) {
		t.Errorf("blocked Reviewer preparation left sessions=%d turns=%d job=%s; want 0, 0, LEASED", reviewerSessions, reviewerTurns, preparationStatus)
	}
}

func TestPrepareAgentTurnRollsBackInvalidProfileAndPreservesPathValidation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000007", "62000000-0000-4000-8000-000000000007")
	lease := claimPreparationJob(t, database, ctx)
	baseConfig := agentProfileConfig("developer", workflow.RoleDeveloper, "opencode-acp/1", "openai/gpt-5.2", "", 40, "Implement safely.", nil)
	for name, invalidate := range map[string]func(*store.AgentTurnPreparationSpec){
		"Developer binding":            func(spec *store.AgentTurnPreparationSpec) { spec.Developer.Binding.RuntimeImageDigest = "" },
		"Reviewer binding":             func(spec *store.AgentTurnPreparationSpec) { spec.Reviewer.Binding.AgentProfileName = "" },
		"missing Runtime Profile hash": func(spec *store.AgentTurnPreparationSpec) { spec.Developer.Binding.RuntimeProfileContentSHA256 = "" },
		"short Runtime Profile hash": func(spec *store.AgentTurnPreparationSpec) {
			spec.Developer.Binding.RuntimeProfileContentSHA256 = "abcd"
		},
		"uppercase Runtime Profile hash": func(spec *store.AgentTurnPreparationSpec) {
			spec.Developer.Binding.RuntimeProfileContentSHA256 = strings.Repeat("A", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := preparationSpec("invalid-binding", "openai/gpt-5.2")
			invalidate(&spec)
			if _, err := database.PrepareAgentTurn(ctx, lease, spec); err == nil {
				t.Fatalf("PrepareAgentTurn() with incomplete %s succeeded", name)
			}
		})
	}
	var decoded map[string]any
	if err := json.Unmarshal(baseConfig, &decoded); err != nil {
		t.Fatal(err)
	}
	invalidConfigs := map[string]func(map[string]any){
		"missing path":       func(config map[string]any) { delete(config, "path") },
		"mismatched path":    func(config map[string]any) { config["path"] = ".omnigrex/team/reviewer.md" },
		"mismatched name":    func(config map[string]any) { config["name"] = "reviewer" },
		"mismatched role":    func(config map[string]any) { config["role"] = workflow.RoleReviewer },
		"mismatched runtime": func(config map[string]any) { config["runtime"] = "opencode-acp/2" },
		"invalid model":      func(config map[string]any) { config["model"] = "gpt-5.2" },
		"invalid steps":      func(config map[string]any) { config["steps"] = 0 },
		"empty permissions":  func(config map[string]any) { config["permissions"] = map[string]string{} },
		"blank instructions": func(config map[string]any) { config["instructions"] = " " },
		"invalid variant":    func(config map[string]any) { config["variant"] = " " },
		"unknown field":      func(config map[string]any) { config["extra"] = true },
		"credential":         func(config map[string]any) { config["permissions"] = map[string]string{"api_token": "allow"} },
	}
	for name, mutate := range invalidConfigs {
		t.Run(name, func(t *testing.T) {
			config := make(map[string]any, len(decoded))
			for key, value := range decoded {
				config[key] = value
			}
			mutate(config)
			encoded, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(name))
			spec := preparationSpec("invalid-profile", "openai/gpt-5.2")
			spec.Developer.Profile = store.AgentProfileSnapshot{CommitSHA: "invalid-profile", ContentSHA256: hash[:], Config: encoded}
			_, err = database.PrepareAgentTurn(ctx, lease, spec)
			if err == nil {
				t.Fatalf("PrepareAgentTurn() with %s succeeded", name)
			}
		})
	}
	for name, profile := range map[string]store.AgentProfileSnapshot{
		"missing commit": {ContentSHA256: make([]byte, 32), Config: baseConfig},
		"invalid digest": {CommitSHA: "invalid-profile", ContentSHA256: []byte("short"), Config: baseConfig},
	} {
		t.Run(name, func(t *testing.T) {
			spec := preparationSpec("invalid-profile", "openai/gpt-5.2")
			spec.Developer.Profile = profile
			if _, err := database.PrepareAgentTurn(ctx, lease, spec); err == nil {
				t.Fatalf("PrepareAgentTurn() with %s succeeded", name)
			}
		})
	}
	assignments, listErr := database.ListAgentAssignments(ctx, application.WorkflowID)
	if listErr != nil || len(assignments) != 0 {
		t.Fatalf("Assignments after rolled-back preparation = (%#v, %v), want none", assignments, listErr)
	}
	job, getErr := database.GetJob(ctx, lease.ID)
	if getErr != nil || job.Status != store.JobLeased {
		t.Fatalf("preparation Job after rollback = (%#v, %v), want LEASED", job, getErr)
	}
	var executionJobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'RUN_AGENT_TURN'`, application.WorkflowID).Scan(&executionJobs); err != nil || executionJobs != 0 {
		t.Fatalf("execution Jobs after rollback = (%d, %v), want none", executionJobs, err)
	}
	prepareTurn(t, database, ctx, lease, "valid-profile", "openai/gpt-5.2")
}

func TestPrepareAgentTurnRejectsUnsafePersistedProfileBeforeAllocatingTurn(t *testing.T) {
	tests := []struct {
		name        string
		role        workflow.Role
		steps       int
		permissions map[string]string
	}{
		{name: "steps above maximum", role: workflow.RoleDeveloper, steps: 1001, permissions: map[string]string{"read": "allow"}},
		{name: "unknown permission tool", role: workflow.RoleDeveloper, steps: 40, permissions: map[string]string{"execute": "allow"}},
		{name: "unknown permission action", role: workflow.RoleDeveloper, steps: 40, permissions: map[string]string{"read": "ask"}},
		{name: "Reviewer edit allowance", role: workflow.RoleReviewer, steps: 40, permissions: map[string]string{"edit": "allow"}},
		{name: "Reviewer patch allowance", role: workflow.RoleReviewer, steps: 40, permissions: map[string]string{"patch": "allow"}},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			sequence := index + 51
			workflowID := fmt.Sprintf("61000000-0000-4000-8000-%012d", sequence)
			attemptID := fmt.Sprintf("62000000-0000-4000-8000-%012d", sequence)
			status, purpose, expectedHeadSHA := workflow.StateDeveloping, workflow.TurnPurposeInitialDevelopment, ""
			if test.role == workflow.RoleReviewer {
				status, purpose, expectedHeadSHA = workflow.StateReviewing, workflow.TurnPurposeReview, "review-head"
			}
			if _, err := pool.Exec(ctx, `
INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status)
VALUES ($1, 9123, 'owner', 'repo', $2, $2, $3)`, workflowID, sequence, status); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `
INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ($1, $2, 1, 'ACTIVE')`, attemptID, workflowID); err != nil {
				t.Fatal(err)
			}
			if test.role == workflow.RoleReviewer {
				proposalID := fmt.Sprintf("63000000-0000-4000-8000-%012d", sequence)
				if _, err := pool.Exec(ctx, `
INSERT INTO change_proposals (
    id, workflow_id, repository_id, repository_owner, repository_name,
    pull_request_id, pull_request_number, status, base_ref, base_sha, head_ref, head_sha
)
VALUES ($1, $2, 9123, 'owner', 'repo', $3, $3, 'OPEN', 'main', 'base', 'feature', 'review-head')`,
					proposalID, workflowID, sequence); err != nil {
					t.Fatal(err)
				}
			}
			insertPreparationJob(t, pool, workflowID, attemptID, 1, workflow.AssignmentGenerationNew, test.role, purpose, expectedHeadSHA, "")

			lease := claimPreparationJob(t, database, ctx)
			spec := preparationSpec("unsafe-persisted-profile", "openai/gpt-5.2")
			profile := &spec.Developer.Profile
			name, role, model, instructions := "developer", workflow.RoleDeveloper, "openai/gpt-5.2", "Perform development."
			if test.role == workflow.RoleReviewer {
				profile = &spec.Reviewer.Profile
				name, role, model, instructions = "reviewer", workflow.RoleReviewer, "anthropic/reviewer", "Perform review."
			}
			profile.Config = agentProfileConfig(name, role, "opencode-acp/1", model, "", test.steps, instructions, test.permissions)

			if _, err := database.PrepareAgentTurn(ctx, lease, spec); !errors.Is(err, agentprofile.ErrInvalidProfile) {
				t.Fatalf("PrepareAgentTurn() error = %v, want Agent Profile policy rejection", err)
			}
			var assignments, sessions, turns, executionJobs int
			if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_assignments WHERE workflow_id = $1),
       (SELECT count(*) FROM agent_sessions AS session
        JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
        WHERE assignment.workflow_id = $1),
       (SELECT count(*) FROM agent_turns WHERE workflow_id = $1),
       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'RUN_AGENT_TURN')`,
				workflowID).Scan(&assignments, &sessions, &turns, &executionJobs); err != nil {
				t.Fatal(err)
			}
			job, err := database.GetJob(ctx, lease.ID)
			if err != nil {
				t.Fatal(err)
			}
			if assignments != 0 || sessions != 0 || turns != 0 || executionJobs != 0 || job.Status != store.JobLeased {
				t.Fatalf("rejected preparation left assignments=%d sessions=%d turns=%d execution jobs=%d job=%s; want 0, 0, 0, 0, LEASED",
					assignments, sessions, turns, executionJobs, job.Status)
			}
		})
	}
}

func TestAcquireAgentTurnAllowsLaterTurnToRecoverUnboundCreatingSession(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000009", "62000000-0000-4000-8000-000000000009")
	first := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "profile-1", "openai/one")
	if _, err := pool.Exec(ctx, `UPDATE agent_turns SET status = 'FAILED', active = FALSE WHERE id = $1`, first.Turn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE jobs SET status = 'SUCCEEDED' WHERE id = $1`, first.Job.ID); err != nil {
		t.Fatal(err)
	}
	setWorkflowRevision(t, pool, application.WorkflowID, 2)
	insertPreparationJob(t, pool, application.WorkflowID, first.Turn.WorkflowAttemptID, 2,
		workflow.AssignmentGenerationCurrent, workflow.RoleDeveloper, workflow.TurnPurposeRequestedChanges, "", "")
	second := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "profile-2", "openai/two")
	if second.Turn.TurnNumber != 2 || second.Session.Status != store.AgentSessionCreating {
		t.Fatalf("second preparation = %#v, want turn 2 on CREATING Session", second)
	}
	job := claimAgentTurnJob(t, database, ctx, second.Turn, time.Second)
	lease, err := database.AcquireAgentTurn(ctx, job, second.Turn.ControlRevision, "runtime", time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() for later CREATING turn error = %v", err)
	}
	if lease.ID != second.Turn.ID || lease.AgentSessionID != first.Session.ID {
		t.Fatalf("AcquireAgentTurn() lease = %#v, want second turn on original Session", lease)
	}
}

func TestFakeHumanControllerFencesAutomationDrainsMutationsAndReturnsControl(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000019", "62000000-0000-4000-8000-000000000019")
	prepared := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "profile-1", "openai/one")
	lease := acquireAndBindTurn(t, database, ctx, prepared, "developer-acp")
	active, err := database.GetAgentSession(ctx, prepared.Session.ID)
	if err != nil {
		t.Fatalf("GetAgentSession() error = %v", err)
	}
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	mutation, err := database.ReserveMutation(ctx, lease, store.MutationSpec{
		OperationID: "github:comment:human-handoff", ToolName: "comment_on_issue", Request: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("ReserveMutation() error = %v", err)
	}
	if _, err := database.StartMutation(ctx, lease, mutation.ID); err != nil {
		t.Fatalf("StartMutation() error = %v", err)
	}
	client := newFakeHumanControllerClient()
	automationCtx, cancelAutomation := context.WithCancel(ctx)
	automationDone := make(chan error, 1)
	go func() {
		_, promptErr := session.NewCoordinator(database).Prompt(automationCtx, session.PromptRequest{
			Lease: lease, Session: active, Client: client, Content: []acp.ContentBlock{acp.TextContent("automation prompt")},
		})
		automationDone <- promptErr
	}()
	<-client.automationStarted
	if _, err := database.TransferAgentSessionControl(ctx, active.ID, active.ControlRevision, store.SessionControlHuman, "human-1"); !errors.Is(err, store.ErrAgentTurnActive) {
		t.Fatalf("TransferAgentSessionControl() during active prompt error = %v, want ErrAgentTurnActive", err)
	}
	cancelAutomation()
	if err := <-automationDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled automation Prompt() error = %v, want context.Canceled", err)
	}
	if err := database.CompleteMutation(ctx, lease, mutation.ID, json.RawMessage(`{"comment_id":42}`)); err != nil {
		t.Fatalf("CompleteMutation() error = %v", err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("CloseMutationAdmission() error = %v", err)
	}
	if err := database.FinalizeAgentTurn(ctx, lease, store.AgentTurnCompletion{Status: store.AgentTurnFailed}); err != nil {
		t.Fatalf("FinalizeAgentTurn() error = %v", err)
	}

	human, err := database.TransferAgentSessionControl(ctx, active.ID, active.ControlRevision, store.SessionControlHuman, "human-1")
	if err != nil {
		t.Fatalf("TransferAgentSessionControl() to human error = %v", err)
	}
	if _, err := session.NewCoordinator(database).Prompt(ctx, session.PromptRequest{
		Lease: lease, Session: active, Client: client, Content: []acp.ContentBlock{acp.TextContent("stale automation")},
	}); !errors.Is(err, store.ErrAgentTurnFenceLost) {
		t.Fatalf("stale automation Prompt() error = %v, want ErrAgentTurnFenceLost", err)
	}
	if human.ControlOwner != store.SessionControlHuman || human.ControlRevision != active.ControlRevision+1 ||
		human.ControllerID != "human-1" || human.ControlAcquiredAt == nil {
		t.Fatalf("human control = %#v", human)
	}
	coordinator := session.NewCoordinator(database)
	staleRevision := human
	staleRevision.ControlRevision--
	if _, err := coordinator.HumanPrompt(ctx, session.HumanPromptRequest{
		Session: staleRevision, Client: client, Content: []acp.ContentBlock{acp.TextContent("stale human revision")},
	}); !errors.Is(err, store.ErrAgentSessionControlFenceLost) {
		t.Fatalf("HumanPrompt() stale revision error = %v, want ErrAgentSessionControlFenceLost", err)
	}
	staleACP := human
	staleACP.ACPSessionID = "stale-acp-session"
	if _, err := coordinator.HumanPrompt(ctx, session.HumanPromptRequest{
		Session: staleACP, Client: client, Content: []acp.ContentBlock{acp.TextContent("stale human ACP identity")},
	}); !errors.Is(err, store.ErrAgentSessionACPConflict) {
		t.Fatalf("HumanPrompt() stale ACP identity error = %v, want ErrAgentSessionACPConflict", err)
	}
	humanDone := make(chan error, 1)
	go func() {
		_, promptErr := coordinator.HumanPrompt(ctx, session.HumanPromptRequest{
			Session: human, Client: client, Content: []acp.ContentBlock{acp.TextContent("human follow-up")},
			LeaseDuration: 80 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond,
		})
		humanDone <- promptErr
	}()
	<-client.humanStarted
	time.Sleep(100 * time.Millisecond)
	if _, err := database.TransferAgentSessionControl(ctx, human.ID, human.ControlRevision, store.SessionControlAutomation, "orchestrator"); !errors.Is(err, store.ErrHumanPromptLeaseActive) {
		t.Fatalf("TransferAgentSessionControl() during human prompt error = %v, want ErrHumanPromptLeaseActive", err)
	}
	blockedSpec := prepared.Turn.AgentTurnSpec
	blockedSpec.ControlRevision = human.ControlRevision
	if _, err := database.AllocateAgentTurn(ctx, blockedSpec); !errors.Is(err, store.ErrAgentTurnHierarchyInactive) {
		t.Fatalf("AllocateAgentTurn() under human control error = %v, want ErrAgentTurnHierarchyInactive", err)
	}
	close(client.releaseHuman)
	if err := <-humanDone; err != nil {
		t.Fatalf("human Prompt() error = %v", err)
	}
	automation, err := database.TransferAgentSessionControl(ctx, human.ID, human.ControlRevision, store.SessionControlAutomation, "orchestrator")
	if err != nil {
		t.Fatalf("TransferAgentSessionControl() to automation error = %v", err)
	}
	client.mutex.Lock()
	historyReplayed, prompts, maxConcurrent := client.historyReplayed, client.prompts, client.maxConcurrentPrompts
	client.mutex.Unlock()
	if !historyReplayed || prompts != 2 || maxConcurrent != 1 {
		t.Fatalf("fake human controller = %#v, want replay before one human prompt and no dual prompts", client)
	}
	if automation.ControlOwner != store.SessionControlAutomation || automation.ControlRevision != human.ControlRevision+1 {
		t.Fatalf("returned control = %#v", automation)
	}
	var promptLeaseCleared bool
	if err := pool.QueryRow(ctx, `
SELECT human_prompt_token IS NULL AND human_prompt_leased_at IS NULL
   AND human_prompt_lease_expires_at IS NULL AND human_prompt_heartbeat_at IS NULL
FROM agent_sessions WHERE id = $1`, human.ID).Scan(&promptLeaseCleared); err != nil || !promptLeaseCleared {
		t.Fatalf("human prompt lease after successful prompt = (%t, %v), want cleared", promptLeaseCleared, err)
	}
}

func TestHumanPromptAdmissionLeaseEnforcesFencesAndFailsClosedAfterExpiry(t *testing.T) {
	databases, _ := openPhaseFiveStores(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	human := prepareHumanControlledSession(t, databases[0], ctx,
		"61000000-0000-4000-8000-000000000020", "62000000-0000-4000-8000-000000000020")

	if _, err := databases[0].AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision-1, human.ACPSessionID, time.Second); !errors.Is(err, store.ErrAgentSessionControlFenceLost) {
		t.Fatalf("AcquireHumanPromptLease() stale revision error = %v, want ErrAgentSessionControlFenceLost", err)
	}
	if _, err := databases[0].AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision, "stale-acp", time.Second); !errors.Is(err, store.ErrAgentSessionACPConflict) {
		t.Fatalf("AcquireHumanPromptLease() stale ACP ID error = %v, want ErrAgentSessionACPConflict", err)
	}

	first, err := databases[0].AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision, human.ACPSessionID, 80*time.Millisecond)
	if err != nil {
		t.Fatalf("AcquireHumanPromptLease() error = %v", err)
	}
	if _, err := databases[1].AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision, human.ACPSessionID, time.Second); !errors.Is(err, store.ErrHumanPromptLeaseActive) {
		t.Fatalf("duplicate AcquireHumanPromptLease() error = %v, want ErrHumanPromptLeaseActive", err)
	}
	if err := databases[0].HeartbeatHumanPromptLease(ctx, first, 180*time.Millisecond); err != nil {
		t.Fatalf("HeartbeatHumanPromptLease() error = %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := databases[1].TransferAgentSessionControl(ctx, human.ID, human.ControlRevision, store.SessionControlAutomation, "orchestrator"); !errors.Is(err, store.ErrHumanPromptLeaseActive) {
		t.Fatalf("TransferAgentSessionControl() during renewed lease error = %v, want ErrHumanPromptLeaseActive", err)
	}
	if err := databases[0].ReleaseHumanPromptLease(ctx, first); err != nil {
		t.Fatalf("ReleaseHumanPromptLease() error = %v", err)
	}

	expiring, err := databases[0].AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision, human.ACPSessionID, 40*time.Millisecond)
	if err != nil {
		t.Fatalf("AcquireHumanPromptLease() for expiry error = %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := databases[1].AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision, human.ACPSessionID, time.Second); !errors.Is(err, store.ErrHumanPromptAdmissionUncertain) {
		t.Fatalf("AcquireHumanPromptLease() after expiry error = %v, want ErrHumanPromptAdmissionUncertain", err)
	}
	if err := databases[0].HeartbeatHumanPromptLease(ctx, expiring, time.Second); !errors.Is(err, store.ErrHumanPromptLeaseLost) {
		t.Fatalf("HeartbeatHumanPromptLease() after expiry error = %v, want ErrHumanPromptLeaseLost", err)
	}
	if _, err := databases[1].TransferAgentSessionControl(ctx, human.ID, human.ControlRevision, store.SessionControlAutomation, "orchestrator"); !errors.Is(err, store.ErrHumanPromptAdmissionUncertain) {
		t.Fatalf("TransferAgentSessionControl() after expiry error = %v, want ErrHumanPromptAdmissionUncertain", err)
	}
	if err := databases[0].ReleaseHumanPromptLease(ctx, expiring); err != nil {
		t.Fatalf("ReleaseHumanPromptLease() for exact expired token error = %v", err)
	}

	afterRelease, err := databases[1].AcquireHumanPromptLease(ctx, human.ID, human.ControlRevision, human.ACPSessionID, time.Second)
	if err != nil {
		t.Fatalf("AcquireHumanPromptLease() after exact release error = %v", err)
	}
	if afterRelease.Token == expiring.Token {
		t.Fatal("new human prompt lease reused the released token")
	}
	if err := databases[1].ReleaseHumanPromptLease(ctx, afterRelease); err != nil {
		t.Fatalf("ReleaseHumanPromptLease() for new lease error = %v", err)
	}
	if _, err := databases[1].TransferAgentSessionControl(ctx, human.ID, human.ControlRevision, store.SessionControlAutomation, "orchestrator"); err != nil {
		t.Fatalf("TransferAgentSessionControl() after exact release error = %v", err)
	}
}

func TestHumanPromptCoordinatorReleasesLeaseAfterACPError(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	human := prepareHumanControlledSession(t, databases[0], ctx,
		"61000000-0000-4000-8000-000000000021", "62000000-0000-4000-8000-000000000021")
	replayClient := newBlockingReplayHumanPromptClient()
	replayDone := make(chan error, 1)
	go func() {
		_, err := session.NewCoordinator(databases[0]).HumanPrompt(ctx, session.HumanPromptRequest{
			Session: human, Client: replayClient, Content: []acp.ContentBlock{acp.TextContent("after replay")},
			LeaseDuration: 80 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond,
		})
		replayDone <- err
	}()
	<-replayClient.replayStarted
	time.Sleep(100 * time.Millisecond)
	if _, err := databases[0].TransferAgentSessionControl(ctx, human.ID, human.ControlRevision, store.SessionControlAutomation, "orchestrator"); !errors.Is(err, store.ErrHumanPromptLeaseActive) {
		t.Fatalf("TransferAgentSessionControl() during history replay error = %v, want ErrHumanPromptLeaseActive", err)
	}
	close(replayClient.releaseReplay)
	if err := <-replayDone; err != nil {
		t.Fatalf("HumanPrompt() after blocked replay error = %v", err)
	}

	want := errors.New("ACP prompt failed")
	client := &failingHumanPromptClient{promptErr: want}

	if _, err := session.NewCoordinator(databases[0]).HumanPrompt(ctx, session.HumanPromptRequest{
		Session: human, Client: client, Content: []acp.ContentBlock{acp.TextContent("fail")},
	}); !errors.Is(err, want) {
		t.Fatalf("HumanPrompt() error = %v, want ACP error", err)
	}
	var promptLeaseCleared bool
	if err := pool.QueryRow(ctx, `SELECT human_prompt_token IS NULL FROM agent_sessions WHERE id = $1`, human.ID).Scan(&promptLeaseCleared); err != nil || !promptLeaseCleared {
		t.Fatalf("human prompt lease after ACP error = (%t, %v), want cleared", promptLeaseCleared, err)
	}
}

func TestHumanPromptCoordinatorCancelsACPWhenHeartbeatLosesFence(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	human := prepareHumanControlledSession(t, databases[0], ctx,
		"61000000-0000-4000-8000-000000000022", "62000000-0000-4000-8000-000000000022")
	client := newFakeHumanControllerClient()
	done := make(chan error, 1)
	go func() {
		_, err := session.NewCoordinator(databases[0]).HumanPrompt(ctx, session.HumanPromptRequest{
			Session: human, Client: client, Content: []acp.ContentBlock{acp.TextContent("human follow-up")},
			LeaseDuration: 150 * time.Millisecond, HeartbeatInterval: 20 * time.Millisecond,
		})
		done <- err
	}()
	<-client.humanStarted
	if _, err := pool.Exec(ctx, `
UPDATE agent_sessions
SET human_prompt_token = '69000000-0000-4000-8000-000000000001'
WHERE id = $1`, human.ID); err != nil {
		t.Fatalf("replace human prompt fence: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrHumanPromptLeaseLost) || !errors.Is(err, session.ErrHumanPromptOutcomeUncertain) {
			t.Fatalf("HumanPrompt() after heartbeat fence loss error = %v, want lease loss and uncertain outcome", err)
		}
	case <-ctx.Done():
		t.Fatal("HumanPrompt() did not cancel ACP after heartbeat fence loss")
	}
}

type fakeHumanControllerClient struct {
	mutex                sync.Mutex
	historyReplayed      bool
	prompts              int
	activePrompts        int
	maxConcurrentPrompts int
	automationStarted    chan struct{}
	humanStarted         chan struct{}
	releaseHuman         chan struct{}
}

type failingHumanPromptClient struct {
	promptErr error
}

type blockingReplayHumanPromptClient struct {
	replayStarted chan struct{}
	releaseReplay chan struct{}
}

func newBlockingReplayHumanPromptClient() *blockingReplayHumanPromptClient {
	return &blockingReplayHumanPromptClient{replayStarted: make(chan struct{}), releaseReplay: make(chan struct{})}
}

func (client *blockingReplayHumanPromptClient) ReplayHistory(ctx context.Context, _ acp.ContinueSessionRequest) error {
	close(client.replayStarted)
	select {
	case <-client.releaseReplay:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (*blockingReplayHumanPromptClient) SetAgentEventContext(agentevent.Context) {}

func (*blockingReplayHumanPromptClient) Prompt(context.Context, string, []acp.ContentBlock) (acp.PromptResponse, error) {
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (*failingHumanPromptClient) ReplayHistory(context.Context, acp.ContinueSessionRequest) error {
	return nil
}

func (*failingHumanPromptClient) SetAgentEventContext(agentevent.Context) {}

func (client *failingHumanPromptClient) Prompt(context.Context, string, []acp.ContentBlock) (acp.PromptResponse, error) {
	return acp.PromptResponse{}, client.promptErr
}

func newFakeHumanControllerClient() *fakeHumanControllerClient {
	return &fakeHumanControllerClient{
		automationStarted: make(chan struct{}), humanStarted: make(chan struct{}), releaseHuman: make(chan struct{}),
	}
}

func (client *fakeHumanControllerClient) ReplayHistory(_ context.Context, _ acp.ContinueSessionRequest) error {
	client.mutex.Lock()
	defer client.mutex.Unlock()
	client.historyReplayed = true
	return nil
}

func (client *fakeHumanControllerClient) SetAgentEventContext(agentevent.Context) {}

func (client *fakeHumanControllerClient) Prompt(ctx context.Context, _ string, content []acp.ContentBlock) (acp.PromptResponse, error) {
	client.mutex.Lock()
	client.activePrompts++
	if client.activePrompts > client.maxConcurrentPrompts {
		client.maxConcurrentPrompts = client.activePrompts
	}
	client.prompts++
	human := len(content) == 1 && content[0].Text == "human follow-up"
	if human && !client.historyReplayed {
		client.activePrompts--
		client.mutex.Unlock()
		return acp.PromptResponse{}, errors.New("history was not replayed")
	}
	client.mutex.Unlock()
	if human {
		close(client.humanStarted)
		select {
		case <-client.releaseHuman:
		case <-ctx.Done():
		}
	} else {
		close(client.automationStarted)
		<-ctx.Done()
	}
	client.mutex.Lock()
	client.activePrompts--
	client.mutex.Unlock()
	if err := ctx.Err(); err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func TestPrepareAgentTurnReusesReturningAndRetainedSessionsAndReplacesNewGeneration(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000008", "62000000-0000-4000-8000-000000000008")
	first := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "profile-1", "openai/one")
	firstLease := acquireAndBindTurn(t, database, ctx, first, "developer-acp")
	settleAcquiredTurn(t, database, ctx, firstLease, store.AgentTurnSucceeded)

	setWorkflowRevision(t, pool, application.WorkflowID, 2)
	insertPreparationJob(t, pool, application.WorkflowID, first.Turn.WorkflowAttemptID, 2,
		workflow.AssignmentGenerationCurrent, workflow.RoleDeveloper, workflow.TurnPurposeRequestedChanges, "", "")
	returningJob := claimPreparationJob(t, database, ctx)
	if _, err := pool.Exec(ctx, `
UPDATE agent_sessions SET runtime_profile_content_sha256 = $2 WHERE id = $1`,
		first.Session.ID, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.PrepareAgentTurn(ctx, returningJob, preparationSpec("profile-2", "openai/two")); !errors.Is(err, store.ErrAssignmentConfigurationConflict) {
		t.Fatalf("PrepareAgentTurn() with Session Runtime Profile hash drift error = %v, want ErrAssignmentConfigurationConflict", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE agent_sessions SET runtime_profile_content_sha256 = $2 WHERE id = $1`,
		first.Session.ID, first.Assignment.RuntimeProfileContentSHA256); err != nil {
		t.Fatal(err)
	}
	returning := prepareTurn(t, database, ctx, returningJob, "profile-2", "openai/two")
	if returning.Assignment.ID != first.Assignment.ID || returning.Session.ID != first.Session.ID ||
		returning.Session.ACPSessionID != "developer-acp" || returning.Turn.ExecutionEpoch != 2 {
		t.Fatalf("returning preparation = %#v, want reused identity and epoch 2", returning)
	}
	returningLease := acquireAndBindTurn(t, database, ctx, returning, "developer-acp")
	settleAcquiredTurn(t, database, ctx, returningLease, store.AgentTurnSucceeded)

	if _, err := pool.Exec(ctx, `
UPDATE agent_assignments SET status = 'COMPLETED', completed_at = clock_timestamp(), retention_until = clock_timestamp() + interval '1 day'
WHERE workflow_id = $1`, application.WorkflowID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'RETAINED', retained_at = clock_timestamp() WHERE id = $1`, first.Session.ID); err != nil {
		t.Fatal(err)
	}
	setWorkflowRevision(t, pool, application.WorkflowID, 3)
	insertPreparationJob(t, pool, application.WorkflowID, first.Turn.WorkflowAttemptID, 3,
		workflow.AssignmentGenerationRetained, workflow.RoleDeveloper, workflow.TurnPurposeRequestedChanges, "", "")
	retained := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "profile-3", "openai/three")
	if retained.Assignment.ID != first.Assignment.ID || retained.Session.ID != first.Session.ID ||
		retained.Session.Status != store.AgentSessionActive || retained.Session.ACPSessionID != "developer-acp" {
		t.Fatalf("retained preparation = %#v, want reactivated durable identity", retained)
	}
	retainedLease := acquireAndBindTurn(t, database, ctx, retained, "developer-acp")
	settleAcquiredTurn(t, database, ctx, retainedLease, store.AgentTurnSucceeded)

	if _, err := pool.Exec(ctx, `UPDATE agent_assignments SET status = 'SUPERSEDED' WHERE workflow_id = $1`, application.WorkflowID); err != nil {
		t.Fatal(err)
	}
	setWorkflowRevision(t, pool, application.WorkflowID, 4)
	insertPreparationJob(t, pool, application.WorkflowID, first.Turn.WorkflowAttemptID, 4,
		workflow.AssignmentGenerationNew, workflow.RoleDeveloper, workflow.TurnPurposeRequestedChanges, "", "")
	newGeneration := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "profile-4", "openai/four")
	if newGeneration.Assignment.ID == first.Assignment.ID || newGeneration.Session.ID == first.Session.ID ||
		newGeneration.Assignment.Generation != 2 || newGeneration.Session.Status != store.AgentSessionCreating {
		t.Fatalf("new-generation preparation = %#v", newGeneration)
	}
	assignments, err := database.ListAgentAssignments(ctx, application.WorkflowID)
	if err != nil || len(assignments) != 4 {
		t.Fatalf("all Assignment generations = (%#v, %v), want four records", assignments, err)
	}
}

func TestPrepareAgentTurnInheritsOperationLineageAcrossRetryChain(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000043", "62000000-0000-4000-8000-000000000043")
	root := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "lineage-root", "openai/root")
	rootLease := acquireAndBindTurn(t, database, ctx, root, "lineage-session")
	settleAcquiredTurn(t, database, ctx, rootLease, store.AgentTurnFailed)

	prepareRetry := func(revision int64, target store.AgentTurn, profile string) store.AgentTurnPreparationCommit {
		t.Helper()
		setWorkflowRevision(t, pool, application.WorkflowID, revision)
		insertPreparationJob(t, pool, application.WorkflowID, root.Turn.WorkflowAttemptID, revision,
			workflow.AssignmentGenerationCurrent, workflow.RoleDeveloper, workflow.TurnPurposeRetry, "", target.ID)
		return prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), profile, "openai/retry")
	}

	firstRetry := prepareRetry(2, root.Turn, "lineage-retry-1")
	firstRetryLease := acquireAndBindTurn(t, database, ctx, firstRetry, "lineage-session")
	settleAcquiredTurn(t, database, ctx, firstRetryLease, store.AgentTurnFailed)
	secondRetry := prepareRetry(3, firstRetry.Turn, "lineage-retry-2")

	var rootLineage, firstRetryLineage, secondRetryLineage string
	if err := pool.QueryRow(ctx, `
SELECT root.operation_lineage_id::text, first_retry.operation_lineage_id::text,
       second_retry.operation_lineage_id::text
FROM agent_turns AS root
JOIN agent_turns AS first_retry ON first_retry.id = $2
JOIN agent_turns AS second_retry ON second_retry.id = $3
WHERE root.id = $1`, root.Turn.ID, firstRetry.Turn.ID, secondRetry.Turn.ID).Scan(
		&rootLineage, &firstRetryLineage, &secondRetryLineage,
	); err != nil {
		t.Fatal(err)
	}
	if rootLineage != root.Turn.ID || firstRetryLineage != root.Turn.ID || secondRetryLineage != root.Turn.ID {
		t.Errorf("prepared operation lineages = root %s, first retry %s, second retry %s; want %s", rootLineage, firstRetryLineage, secondRetryLineage, root.Turn.ID)
	}
}

func TestRuntimeProfilePlatformDriftCreatesConfigurationConflictAndHumanHandoff(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000011", "62000000-0000-4000-8000-000000000011")
	const image = "registry.example/omnigrex/opencode@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	amd64Profile, err := runtimeprofile.NewOpenCodeV1(image, runtimeprofile.Platform{OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	arm64Profile, err := runtimeprofile.NewOpenCodeV1(image, runtimeprofile.Platform{OS: "linux", Arch: "arm64"})
	if err != nil {
		t.Fatal(err)
	}
	if amd64Profile.ContentSHA256() == arm64Profile.ContentSHA256() {
		t.Fatal("Runtime Profiles for different platforms have the same content hash")
	}
	bindings := preparationBindings()
	developerBinding := bindings[workflow.RoleDeveloper]
	developerBinding.RuntimeImageDigest = image
	developerBinding.RuntimeProfileContentSHA256 = amd64Profile.ContentSHA256()
	bindings[workflow.RoleDeveloper] = developerBinding
	for index, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		binding := bindings[role]
		assignmentID := fmt.Sprintf("63000000-0000-4000-8000-%012d", 11+index)
		if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_profile_content_sha256, runtime_image_digest, runtime_state_path
)
VALUES ($1, $2, $3, 'ACTIVE', $4, $5, $6, $7, $8, $9)`, assignmentID,
			application.WorkflowID, role, binding.AgentProfileName, binding.RuntimeProfileName,
			binding.RuntimeProfileVersion, binding.RuntimeProfileContentSHA256,
			binding.RuntimeImageDigest, "/state/"+assignmentID); err != nil {
			t.Fatalf("seed %s Assignment: %v", role, err)
		}
	}
	if _, err := pool.Exec(ctx, `
UPDATE jobs
SET payload = jsonb_set(payload, '{mode}', '"CURRENT"'::jsonb)
WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'`, application.WorkflowID); err != nil {
		t.Fatalf("prepare current-generation Job: %v", err)
	}
	lease := claimPreparationJob(t, database, ctx)
	matching := preparationSpec("matching-profile", "openai/matching")
	matching.Developer.Binding = developerBinding
	conflicting := matching
	conflicting.Developer.Binding.RuntimeProfileContentSHA256 = arm64Profile.ContentSHA256()

	if _, err := database.PrepareAgentTurn(ctx, lease, conflicting); !errors.Is(err, store.ErrAssignmentConfigurationConflict) {
		t.Fatalf("PrepareAgentTurn() error = %v, want ErrAssignmentConfigurationConflict", err)
	}
	if _, err := database.AcknowledgeAssignmentConfigurationConflict(ctx, lease, matching); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
		t.Fatalf("acknowledgement without a current conflict error = %v, want ErrAgentTurnPreparationFenceLost", err)
	}
	stale := lease
	stale.LeaseOwner = "stale-preparation-worker"
	if _, err := database.AcknowledgeAssignmentConfigurationConflict(ctx, stale, conflicting); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
		t.Fatalf("stale acknowledgement error = %v, want ErrAgentTurnPreparationFenceLost", err)
	}

	handoff, err := database.AcknowledgeAssignmentConfigurationConflict(ctx, lease, conflicting)
	if err != nil {
		t.Fatalf("AcknowledgeAssignmentConfigurationConflict() error = %v", err)
	}
	if handoff.JobID != lease.ID || handoff.WorkflowID != application.WorkflowID ||
		handoff.WorkflowAttemptID != lease.WorkflowAttemptID || handoff.Role != workflow.RoleDeveloper ||
		handoff.WorkflowRevision != 2 {
		t.Fatalf("configuration handoff = %#v", handoff)
	}
	if _, err := database.AcknowledgeAssignmentConfigurationConflict(ctx, lease, conflicting); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
		t.Fatalf("duplicate acknowledgement error = %v, want ErrAgentTurnPreparationFenceLost", err)
	}

	var workflowStatus, desiredAssignmentStatus, workflowReason, attemptReason, preparationStatus, preparationAttemptStatus string
	var revision int64
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.state_revision, workflow.desired_assignment_status,
       workflow.human_handoff_reason, attempt.human_handoff_reason, preparation.status,
       preparation_attempt.status
FROM workflows AS workflow
JOIN workflow_attempts AS attempt ON attempt.id = $2
JOIN jobs AS preparation ON preparation.id = $3
JOIN job_attempts AS preparation_attempt
  ON preparation_attempt.job_id = preparation.id AND preparation_attempt.attempt_number = $4
WHERE workflow.id = $1`, application.WorkflowID, lease.WorkflowAttemptID, lease.ID, lease.Attempt).Scan(
		&workflowStatus, &revision, &desiredAssignmentStatus, &workflowReason, &attemptReason,
		&preparationStatus, &preparationAttemptStatus,
	); err != nil {
		t.Fatalf("read durable configuration handoff: %v", err)
	}
	if workflowStatus != string(workflow.StateNeedsHuman) || revision != 2 ||
		desiredAssignmentStatus != string(workflow.AssignmentWaitingForHuman) ||
		workflowReason != string(workflow.ReasonAssignmentConfigurationConflict) ||
		attemptReason != string(workflow.ReasonAssignmentConfigurationConflict) ||
		preparationStatus != string(store.JobSucceeded) || preparationAttemptStatus != string(store.JobSucceeded) {
		t.Errorf("durable handoff = state %s@%d, desired %s, reasons %s/%s, preparation %s/%s",
			workflowStatus, revision, desiredAssignmentStatus, workflowReason, attemptReason,
			preparationStatus, preparationAttemptStatus)
	}
	var waitingAssignments, unchangedBindings, turns, runJobs int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE status = 'WAITING_FOR_HUMAN'),
       count(*) FILTER (WHERE runtime_profile_content_sha256 <> $2)
FROM agent_assignments WHERE workflow_id = $1`, application.WorkflowID, arm64Profile.ContentSHA256()).Scan(&waitingAssignments, &unchangedBindings); err != nil {
		t.Fatalf("read acknowledged Assignments: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_turns WHERE workflow_id = $1`, application.WorkflowID).Scan(&turns); err != nil {
		t.Fatalf("count Agent Turns: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'RUN_AGENT_TURN'`, application.WorkflowID).Scan(&runJobs); err != nil {
		t.Fatalf("count execution Jobs: %v", err)
	}
	if waitingAssignments != 2 || unchangedBindings != 2 || turns != 0 || runJobs != 0 {
		t.Errorf("conflict acknowledgement left %d waiting Assignments, %d unchanged bindings, %d Turns, and %d execution Jobs; want 2, 2, 0, and 0",
			waitingAssignments, unchangedBindings, turns, runJobs)
	}

	namespace := "prepare-agent-turn:" + lease.ID + ":"
	rows, err := pool.Query(ctx, `
SELECT kind, action_key, idempotency_key, normalized_event_id::text, payload::text
FROM jobs
WHERE workflow_id = $1 AND action_key LIKE $2
ORDER BY kind`, application.WorkflowID, namespace+"%")
	if err != nil {
		t.Fatalf("read derived outbox Jobs: %v", err)
	}
	defer rows.Close()
	derived := map[string]string{}
	derivedCount := 0
	for rows.Next() {
		var kind, actionKey, idempotencyKey, normalizedEventID, payload string
		if err := rows.Scan(&kind, &actionKey, &idempotencyKey, &normalizedEventID, &payload); err != nil {
			t.Fatalf("scan derived outbox Job: %v", err)
		}
		derivedCount++
		if !strings.HasPrefix(actionKey, namespace) || !strings.Contains(idempotencyKey, ":action:"+namespace) {
			t.Errorf("derived outbox keys = %q / %q, want namespace %q", actionKey, idempotencyKey, namespace)
		}
		if normalizedEventID != application.DeliveryID {
			t.Errorf("derived outbox normalized event = %q, want %q", normalizedEventID, application.DeliveryID)
		}
		if strings.Contains(payload, arm64Profile.ContentSHA256()) {
			t.Errorf("derived outbox payload exposed conflicting configuration: %s", payload)
		}
		derived[kind] = payload
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate derived outbox Jobs: %v", err)
	}
	if derivedCount != 2 || len(derived) != 2 || derived["PUBLISH_HUMAN_HANDOFF"] == "" || derived["RECONCILE_GITHUB_LABELS"] == "" {
		t.Errorf("derived outbox Jobs = %#v, want one handoff and one label reconciliation", derived)
	}
}

func TestPreparationWorkerPreservesConfigurationConflictHandoffAcrossUnresolvedRetriggers(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000031", "62000000-0000-4000-8000-000000000032")
	bindings := preparationBindings()
	assignmentIDs := map[workflow.Role]string{
		workflow.RoleDeveloper: "63000000-0000-4000-8000-000000000032",
		workflow.RoleReviewer:  "63000000-0000-4000-8000-000000000033",
	}
	for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		binding := bindings[role]
		if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_profile_content_sha256, runtime_image_digest, runtime_state_path
)
VALUES ($1, $2, $3, 'ACTIVE', $4, $5, $6, $7, $8, $9)`, assignmentIDs[role],
			application.WorkflowID, role, binding.AgentProfileName, binding.RuntimeProfileName,
			binding.RuntimeProfileVersion, binding.RuntimeProfileContentSHA256,
			binding.RuntimeImageDigest, "repeated-conflict/"+strings.ToLower(string(role))); err != nil {
			t.Fatalf("seed %s Assignment: %v", role, err)
		}
	}
	if _, err := pool.Exec(ctx, `
UPDATE jobs SET payload = jsonb_set(payload, '{mode}', '"CURRENT"'::jsonb)
WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'`, application.WorkflowID); err != nil {
		t.Fatalf("prepare current-generation Job: %v", err)
	}

	conflicting := preparationSpec("repeated-conflict-profile", "openai/repeated-conflict")
	conflicting.Developer.Binding.RuntimeImageDigest = "sha256:unresolved-developer-runtime"
	preparer := integrationTurnPreparerFunc(func(context.Context, agentturn.Request) (agentturn.Result, error) {
		return agentturn.Result{}, &agentturn.AssignmentConfigurationConflictError{
			Preparation: conflicting,
			Cause:       store.ErrAssignmentConfigurationConflict,
		}
	})
	worker, err := agentturn.NewWorker(database,
		&integrationCredentialProvider{credential: "developer-installation-token"},
		&integrationCredentialProvider{credential: "reviewer-installation-token"},
		preparer,
		agentturn.WorkerConfig{
			ClaimOwner: "repeated-conflict-worker", LeaseDuration: 5 * time.Second,
			HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, RetryDelay: time.Second,
		})
	if err != nil {
		t.Fatal(err)
	}

	assertConflictHandoff := func(attemptID string) {
		t.Helper()
		processed, err := worker.ProcessNext(ctx)
		if err != nil || !processed {
			t.Fatalf("ProcessNext() = (%t, %v), want acknowledged configuration conflict", processed, err)
		}

		var workflowStatus, desiredAssignmentStatus, workflowReason, attemptReason string
		var preparationStatus, preparationReason, handoffReason, handoffDiagnostic string
		if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.desired_assignment_status, workflow.human_handoff_reason,
       attempt.human_handoff_reason, preparation.status, preparation.result->>'reason',
       handoff.payload->>'reason', handoff.payload->>'diagnostic'
FROM workflows AS workflow
JOIN workflow_attempts AS attempt ON attempt.id = $2
JOIN jobs AS preparation
  ON preparation.workflow_attempt_id = attempt.id AND preparation.kind = 'PREPARE_AGENT_TURN'
JOIN jobs AS handoff
  ON handoff.workflow_attempt_id = attempt.id
 AND handoff.action_key = 'prepare-agent-turn:' || preparation.id::text || ':publish-human-handoff'
WHERE workflow.id = $1`, application.WorkflowID, attemptID).Scan(
			&workflowStatus, &desiredAssignmentStatus, &workflowReason, &attemptReason,
			&preparationStatus, &preparationReason, &handoffReason, &handoffDiagnostic,
		); err != nil {
			t.Fatalf("read durable configuration conflict handoff: %v", err)
		}
		wantReason := string(workflow.ReasonAssignmentConfigurationConflict)
		if workflowStatus != string(workflow.StateNeedsHuman) ||
			desiredAssignmentStatus != string(workflow.AssignmentWaitingForHuman) ||
			workflowReason != wantReason || attemptReason != wantReason ||
			preparationStatus != string(store.JobSucceeded) || preparationReason != wantReason ||
			handoffReason != wantReason || handoffDiagnostic != "" {
			t.Errorf("configuration conflict handoff = Workflow %s desired %s reasons %s/%s, preparation %s/%s, published %s diagnostic %q",
				workflowStatus, desiredAssignmentStatus, workflowReason, attemptReason,
				preparationStatus, preparationReason, handoffReason, handoffDiagnostic)
		}

		assignments, err := database.ListAgentAssignments(ctx, application.WorkflowID)
		if err != nil || len(assignments) != 2 {
			t.Fatalf("ListAgentAssignments() = (%#v, %v), want exact Role pair", assignments, err)
		}
		for _, assignment := range assignments {
			if assignment.ID != assignmentIDs[assignment.Role] ||
				assignment.Status != store.AgentAssignmentWaitingForHuman ||
				assignment.AssignmentRuntimeBinding != bindings[assignment.Role] {
				t.Errorf("configuration-conflicted Assignment = %#v, want unchanged waiting %s", assignment, assignmentIDs[assignment.Role])
			}
		}
	}

	assertConflictHandoff("62000000-0000-4000-8000-000000000032")
	for _, retrigger := range []struct {
		deliveryID string
		attemptID  string
	}{
		{deliveryID: "61000000-0000-4000-8000-000000000032", attemptID: "62000000-0000-4000-8000-000000000033"},
		{deliveryID: "61000000-0000-4000-8000-000000000033", attemptID: "62000000-0000-4000-8000-000000000034"},
	} {
		triggerPreparationWorkflow(t, database, ctx, retrigger.deliveryID, retrigger.attemptID)
		assertConflictHandoff(retrigger.attemptID)
	}

	var conflictHandoffs, genericPreparationHandoffs int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE human_handoff_reason = $2),
       count(*) FILTER (WHERE human_handoff_reason = $3)
FROM workflow_attempts WHERE workflow_id = $1`, application.WorkflowID,
		workflow.ReasonAssignmentConfigurationConflict, workflow.ReasonAgentTurnPreparationFailed).Scan(
		&conflictHandoffs, &genericPreparationHandoffs,
	); err != nil {
		t.Fatal(err)
	}
	if conflictHandoffs != 3 || genericPreparationHandoffs != 0 {
		t.Errorf("durable Human Handoffs = %d configuration conflicts and %d generic preparation failures, want 3 and 0",
			conflictHandoffs, genericPreparationHandoffs)
	}
}

func TestPermanentAgentTurnPreparationFailureAtomicallyCreatesHumanHandoff(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000021", "62000000-0000-4000-8000-000000000021")
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_profile_content_sha256, runtime_image_digest, runtime_state_path
)
VALUES ('63000000-0000-4000-8000-000000000021', $1, 'DEVELOPER', 'ACTIVE',
        'developer', 'opencode-acp', '1', repeat('a', 64), 'sha256:developer', 'failure/developer'),
       ('63000000-0000-4000-8000-000000000022', $1, 'REVIEWER', 'ACTIVE',
        'reviewer', 'opencode-acp', '1', repeat('b', 64), 'sha256:reviewer', 'failure/reviewer')`, application.WorkflowID); err != nil {
		t.Fatalf("seed relevant Assignments: %v", err)
	}
	lease := claimPreparationJob(t, database, ctx)
	diagnostic := errors.New("Reviewer GitHub App is not installed")
	stale := lease
	stale.LeaseOwner = "stale-preparation-worker"
	if _, err := database.AcknowledgeAgentTurnPreparationFailure(ctx, stale, diagnostic, false, time.Second); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
		t.Fatalf("stale acknowledgement error = %v, want ErrAgentTurnPreparationFenceLost", err)
	}

	acknowledgement, err := database.AcknowledgeAgentTurnPreparationFailure(ctx, lease, diagnostic, false, time.Second)
	if err != nil {
		t.Fatalf("AcknowledgeAgentTurnPreparationFailure() error = %v", err)
	}
	if acknowledgement.JobID != lease.ID || acknowledgement.WorkflowID != application.WorkflowID ||
		acknowledgement.WorkflowAttemptID != lease.WorkflowAttemptID || acknowledgement.RetryScheduled ||
		acknowledgement.WorkflowRevision != 2 {
		t.Fatalf("preparation failure acknowledgement = %#v", acknowledgement)
	}

	var workflowStatus, desiredAssignmentStatus, workflowReason, attemptReason, jobStatus, attemptStatus, jobError, attemptError string
	var revision int64
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.state_revision, workflow.desired_assignment_status,
       workflow.human_handoff_reason, attempt.human_handoff_reason,
       preparation.status, preparation_attempt.status,
       preparation.last_error, preparation_attempt.last_error
FROM workflows AS workflow
JOIN workflow_attempts AS attempt ON attempt.id = $2
JOIN jobs AS preparation ON preparation.id = $3
JOIN job_attempts AS preparation_attempt
  ON preparation_attempt.job_id = preparation.id AND preparation_attempt.attempt_number = $4
WHERE workflow.id = $1`, application.WorkflowID, lease.WorkflowAttemptID, lease.ID, lease.Attempt).Scan(
		&workflowStatus, &revision, &desiredAssignmentStatus, &workflowReason, &attemptReason,
		&jobStatus, &attemptStatus, &jobError, &attemptError,
	); err != nil {
		t.Fatalf("read durable preparation failure handoff: %v", err)
	}
	if workflowStatus != string(workflow.StateNeedsHuman) || revision != 2 ||
		desiredAssignmentStatus != string(workflow.AssignmentWaitingForHuman) ||
		workflowReason != string(workflow.ReasonAgentTurnPreparationFailed) ||
		attemptReason != string(workflow.ReasonAgentTurnPreparationFailed) ||
		jobStatus != string(store.JobFailed) || attemptStatus != string(store.JobFailed) ||
		jobError != diagnostic.Error() || attemptError != diagnostic.Error() {
		t.Errorf("durable preparation failure = state %s@%d desired %s reasons %s/%s job %s/%s errors %q/%q",
			workflowStatus, revision, desiredAssignmentStatus, workflowReason, attemptReason,
			jobStatus, attemptStatus, jobError, attemptError)
	}
	var turns, executionJobs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_turns WHERE workflow_id = $1`, application.WorkflowID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'RUN_AGENT_TURN'`, application.WorkflowID).Scan(&executionJobs); err != nil {
		t.Fatal(err)
	}
	if turns != 0 || executionJobs != 0 {
		t.Errorf("terminal preparation failure created %d Agent Turns and %d execution Jobs", turns, executionJobs)
	}
	var waitingAssignments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_assignments WHERE workflow_id = $1 AND status = 'WAITING_FOR_HUMAN'`, application.WorkflowID).Scan(&waitingAssignments); err != nil {
		t.Fatal(err)
	}
	if waitingAssignments != 2 {
		t.Errorf("waiting Assignments = %d, want 2", waitingAssignments)
	}
	namespace := "prepare-agent-turn:" + lease.ID + ":"
	var outboxCount int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM jobs
WHERE workflow_id = $1 AND action_key LIKE $2
  AND kind IN ('PUBLISH_HUMAN_HANDOFF', 'RECONCILE_GITHUB_LABELS')`, application.WorkflowID, namespace+"%").Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 2 {
		t.Errorf("terminal preparation outbox count = %d, want 2", outboxCount)
	}
	if _, err := database.AcknowledgeAgentTurnPreparationFailure(ctx, lease, diagnostic, false, time.Second); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
		t.Errorf("duplicate acknowledgement error = %v, want ErrAgentTurnPreparationFenceLost", err)
	}
}

func TestPreparationFailureHandoffRetriggerReactivatesExistingAssignments(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000026", "62000000-0000-4000-8000-000000000026")
	bindings := preparationBindings()
	assignmentIDs := map[workflow.Role]string{
		workflow.RoleDeveloper: "63000000-0000-4000-8000-000000000026",
		workflow.RoleReviewer:  "63000000-0000-4000-8000-000000000027",
	}
	for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		binding := bindings[role]
		if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_profile_content_sha256, runtime_image_digest, runtime_state_path
)
VALUES ($1, $2, $3, 'ACTIVE', $4, $5, $6, $7, $8, $9)`, assignmentIDs[role],
			application.WorkflowID, role, binding.AgentProfileName, binding.RuntimeProfileName,
			binding.RuntimeProfileVersion, binding.RuntimeProfileContentSHA256,
			binding.RuntimeImageDigest, "retrigger/"+strings.ToLower(string(role))); err != nil {
			t.Fatalf("seed %s Assignment: %v", role, err)
		}
	}
	failed := claimPreparationJob(t, database, ctx)
	if _, err := database.AcknowledgeAgentTurnPreparationFailure(ctx, failed, errors.New("preparation dependency is unavailable"), false, time.Second); err != nil {
		t.Fatalf("AcknowledgeAgentTurnPreparationFailure() error = %v", err)
	}

	triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000027", "62000000-0000-4000-8000-000000000028")
	retriggered := claimPreparationJob(t, database, ctx)
	drifted := preparationSpec("retriggered-profile", "openai/retriggered")
	drifted.Developer.Binding.RuntimeImageDigest = "sha256:drifted-developer-runtime"
	if _, err := database.PrepareAgentTurn(ctx, retriggered, drifted); !errors.Is(err, store.ErrAssignmentConfigurationConflict) {
		t.Fatalf("PrepareAgentTurn() with binding drift error = %v, want ErrAssignmentConfigurationConflict", err)
	}
	var waiting, reactivated int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE status = 'WAITING_FOR_HUMAN'),
       count(*) FILTER (WHERE reactivated_by_preparation_job_id IS NOT NULL)
FROM agent_assignments WHERE workflow_id = $1`, application.WorkflowID).Scan(&waiting, &reactivated); err != nil {
		t.Fatal(err)
	}
	if waiting != 2 || reactivated != 0 {
		t.Fatalf("Assignments after drift = %d waiting and %d reactivated, want 2 and 0", waiting, reactivated)
	}

	prepared := prepareTurn(t, database, ctx, retriggered, "retriggered-profile", "openai/retriggered")
	if prepared.Assignment.ID != assignmentIDs[workflow.RoleDeveloper] || prepared.Assignment.Generation != 1 ||
		prepared.Assignment.Status != store.AgentAssignmentActive || prepared.Turn.Purpose != workflow.TurnPurposeReactivation {
		t.Fatalf("retriggered preparation = %#v", prepared)
	}
	assignments, err := database.ListAgentAssignments(ctx, application.WorkflowID)
	if err != nil || len(assignments) != 2 {
		t.Fatalf("ListAgentAssignments() = (%#v, %v), want original pair", assignments, err)
	}
	for _, assignment := range assignments {
		if assignment.ID != assignmentIDs[assignment.Role] || assignment.Status != store.AgentAssignmentActive {
			t.Errorf("reactivated Assignment = %#v, want original active identity", assignment)
		}
	}
	var correctlyReactivated, turns int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (
           WHERE status = 'ACTIVE' AND reactivated_by_preparation_job_id = $2
       ),
       (SELECT count(*) FROM agent_turns WHERE workflow_id = $1)
FROM agent_assignments WHERE workflow_id = $1`, application.WorkflowID, retriggered.ID).Scan(&correctlyReactivated, &turns); err != nil {
		t.Fatal(err)
	}
	if correctlyReactivated != 2 || turns != 1 {
		t.Errorf("retrigger result = %d Assignments reactivated by new preparation and %d Turns, want 2 and 1", correctlyReactivated, turns)
	}
	if _, err := database.PrepareAgentTurn(ctx, retriggered, preparationSpec("retriggered-profile", "openai/retriggered")); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
		t.Fatalf("duplicate PrepareAgentTurn() error = %v, want ErrAgentTurnPreparationFenceLost", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_turns WHERE workflow_id = $1`, application.WorkflowID).Scan(&turns); err != nil || turns != 1 {
		t.Fatalf("Turns after duplicate preparation = (%d, %v), want 1", turns, err)
	}
}

func TestConfigurationConflictHandoffRetriggerReactivatesExistingAssignments(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000028", "62000000-0000-4000-8000-000000000029")
	bindings := preparationBindings()
	assignmentIDs := map[workflow.Role]string{
		workflow.RoleDeveloper: "63000000-0000-4000-8000-000000000028",
		workflow.RoleReviewer:  "63000000-0000-4000-8000-000000000029",
	}
	for _, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		binding := bindings[role]
		if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_profile_content_sha256, runtime_image_digest, runtime_state_path
)
VALUES ($1, $2, $3, 'ACTIVE', $4, $5, $6, $7, $8, $9)`, assignmentIDs[role],
			application.WorkflowID, role, binding.AgentProfileName, binding.RuntimeProfileName,
			binding.RuntimeProfileVersion, binding.RuntimeProfileContentSHA256,
			binding.RuntimeImageDigest, "configuration-retrigger/"+strings.ToLower(string(role))); err != nil {
			t.Fatalf("seed %s Assignment: %v", role, err)
		}
	}
	if _, err := pool.Exec(ctx, `
UPDATE jobs SET payload = jsonb_set(payload, '{mode}', '"CURRENT"'::jsonb)
WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'`, application.WorkflowID); err != nil {
		t.Fatalf("prepare current-generation Job: %v", err)
	}
	conflicted := claimPreparationJob(t, database, ctx)
	conflicting := preparationSpec("conflicting-profile", "openai/conflicting")
	conflicting.Developer.Binding.RuntimeImageDigest = "sha256:conflicting-developer-runtime"
	if _, err := database.PrepareAgentTurn(ctx, conflicted, conflicting); !errors.Is(err, store.ErrAssignmentConfigurationConflict) {
		t.Fatalf("PrepareAgentTurn() error = %v, want ErrAssignmentConfigurationConflict", err)
	}
	if _, err := database.AcknowledgeAssignmentConfigurationConflict(ctx, conflicted, conflicting); err != nil {
		t.Fatalf("AcknowledgeAssignmentConfigurationConflict() error = %v", err)
	}

	triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000029", "62000000-0000-4000-8000-000000000030")
	retriggered := claimPreparationJob(t, database, ctx)
	prepared := prepareTurn(t, database, ctx, retriggered, "resolved-profile", "openai/resolved")
	if prepared.Assignment.ID != assignmentIDs[workflow.RoleDeveloper] || prepared.Assignment.Status != store.AgentAssignmentActive {
		t.Fatalf("retriggered preparation = %#v, want original Developer Assignment", prepared)
	}
	assignments, err := database.ListAgentAssignments(ctx, application.WorkflowID)
	if err != nil || len(assignments) != 2 {
		t.Fatalf("ListAgentAssignments() = (%#v, %v), want original pair", assignments, err)
	}
	for _, assignment := range assignments {
		if assignment.ID != assignmentIDs[assignment.Role] || assignment.Status != store.AgentAssignmentActive {
			t.Errorf("reactivated Assignment = %#v, want original active identity", assignment)
		}
	}
	var correctlyReactivated int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FROM agent_assignments
WHERE workflow_id = $1 AND status = 'ACTIVE' AND reactivated_by_preparation_job_id = $2`,
		application.WorkflowID, retriggered.ID).Scan(&correctlyReactivated); err != nil {
		t.Fatal(err)
	}
	if correctlyReactivated != 2 {
		t.Errorf("Assignments reactivated by new preparation = %d, want 2", correctlyReactivated)
	}
}

func TestCurrentPreparationRejectsUnrelatedWaitingAssignments(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000030", "62000000-0000-4000-8000-000000000031")
	bindings := preparationBindings()
	for index, role := range []workflow.Role{workflow.RoleDeveloper, workflow.RoleReviewer} {
		binding := bindings[role]
		if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_profile_content_sha256, runtime_image_digest, runtime_state_path
)
VALUES ($1, $2, $3, 'WAITING_FOR_HUMAN', $4, $5, $6, $7, $8, $9)`,
			fmt.Sprintf("63000000-0000-4000-8000-%012d", 30+index), application.WorkflowID, role,
			binding.AgentProfileName, binding.RuntimeProfileName, binding.RuntimeProfileVersion,
			binding.RuntimeProfileContentSHA256, binding.RuntimeImageDigest,
			"unrelated-waiting/"+strings.ToLower(string(role))); err != nil {
			t.Fatalf("seed %s Assignment: %v", role, err)
		}
	}
	if _, err := pool.Exec(ctx, `
UPDATE jobs SET payload = jsonb_set(payload, '{mode}', '"CURRENT"'::jsonb)
WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'`, application.WorkflowID); err != nil {
		t.Fatalf("prepare current-generation Job: %v", err)
	}
	preparation := claimPreparationJob(t, database, ctx)
	if _, err := database.PrepareAgentTurn(ctx, preparation, preparationSpec("unrelated-waiting", "openai/waiting")); !errors.Is(err, store.ErrAgentTurnPreparationFenceLost) {
		t.Fatalf("PrepareAgentTurn() error = %v, want ErrAgentTurnPreparationFenceLost", err)
	}
	var waiting, reactivated, turns int
	if err := pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE status = 'WAITING_FOR_HUMAN'),
       count(*) FILTER (WHERE reactivated_by_preparation_job_id IS NOT NULL),
       (SELECT count(*) FROM agent_turns WHERE workflow_id = $1)
FROM agent_assignments WHERE workflow_id = $1`, application.WorkflowID).Scan(&waiting, &reactivated, &turns); err != nil {
		t.Fatal(err)
	}
	if waiting != 2 || reactivated != 0 || turns != 0 {
		t.Errorf("unrelated waiting state = %d waiting, %d reactivated, %d Turns; want 2, 0, 0", waiting, reactivated, turns)
	}
}

func TestTransientAgentTurnPreparationFailureRetriesThenAtomicallyCreatesHumanHandoff(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000022", "62000000-0000-4000-8000-000000000022")
	if _, err := pool.Exec(ctx, `UPDATE jobs SET max_attempts = 2 WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN'`, application.WorkflowID); err != nil {
		t.Fatal(err)
	}
	first := claimPreparationJob(t, database, ctx)
	retryDelay := 100 * time.Millisecond
	acknowledgement, err := database.AcknowledgeAgentTurnPreparationFailure(ctx, first, errors.New("GitHub temporarily unavailable"), true, retryDelay)
	if err != nil {
		t.Fatalf("first AcknowledgeAgentTurnPreparationFailure() error = %v", err)
	}
	if !acknowledgement.RetryScheduled || acknowledgement.WorkflowRevision != 1 {
		t.Fatalf("first preparation failure acknowledgement = %#v, want retry at revision 1", acknowledgement)
	}
	var workflowStatus, jobStatus string
	var revision, retryDelayMicros int64
	var handoffReason *string
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.state_revision, workflow.human_handoff_reason,
       preparation.status,
       (EXTRACT(EPOCH FROM (preparation.available_at - preparation.updated_at)) * 1000000)::bigint
FROM workflows AS workflow
JOIN jobs AS preparation ON preparation.workflow_id = workflow.id AND preparation.kind = 'PREPARE_AGENT_TURN'
WHERE workflow.id = $1`, application.WorkflowID).Scan(&workflowStatus, &revision, &handoffReason, &jobStatus, &retryDelayMicros); err != nil {
		t.Fatal(err)
	}
	if workflowStatus != string(workflow.StateDeveloping) || revision != 1 || handoffReason != nil ||
		jobStatus != string(store.JobAvailable) || retryDelayMicros < retryDelay.Microseconds()-10_000 {
		t.Errorf("scheduled retry = Workflow %s@%d reason %v job %s delay %dus",
			workflowStatus, revision, handoffReason, jobStatus, retryDelayMicros)
	}

	time.Sleep(retryDelay + 25*time.Millisecond)
	second := claimPreparationJob(t, database, ctx)
	if second.Attempt != 2 {
		t.Fatalf("second preparation attempt = %d, want 2", second.Attempt)
	}
	exhausted := errors.New("GitHub remains unavailable")
	acknowledgement, err = database.AcknowledgeAgentTurnPreparationFailure(ctx, second, exhausted, true, retryDelay)
	if err != nil {
		t.Fatalf("exhausted AcknowledgeAgentTurnPreparationFailure() error = %v", err)
	}
	if acknowledgement.RetryScheduled || acknowledgement.WorkflowRevision != 2 {
		t.Fatalf("exhausted preparation failure acknowledgement = %#v, want terminal revision 2", acknowledgement)
	}
	var workflowReason, attemptReason, lastError string
	var attemptRetryable bool
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.state_revision, workflow.human_handoff_reason,
       attempt.human_handoff_reason, preparation.status, preparation.last_error,
       preparation_attempt.retryable
FROM workflows AS workflow
JOIN workflow_attempts AS attempt ON attempt.id = $2
JOIN jobs AS preparation ON preparation.id = $3
JOIN job_attempts AS preparation_attempt
  ON preparation_attempt.job_id = preparation.id AND preparation_attempt.attempt_number = $4
WHERE workflow.id = $1`, application.WorkflowID, second.WorkflowAttemptID, second.ID, second.Attempt).Scan(
		&workflowStatus, &revision, &workflowReason, &attemptReason, &jobStatus, &lastError, &attemptRetryable,
	); err != nil {
		t.Fatal(err)
	}
	if workflowStatus != string(workflow.StateNeedsHuman) || revision != 2 ||
		workflowReason != string(workflow.ReasonAgentTurnPreparationFailed) ||
		attemptReason != string(workflow.ReasonAgentTurnPreparationFailed) ||
		jobStatus != string(store.JobFailed) || lastError != exhausted.Error() || !attemptRetryable {
		t.Errorf("exhausted retry = Workflow %s@%d reasons %s/%s job %s retryable %t error %q",
			workflowStatus, revision, workflowReason, attemptReason, jobStatus, attemptRetryable, lastError)
	}
}

func TestPreparationWorkerDurablyHandsOffMissingReviewerInstallation(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000023", "62000000-0000-4000-8000-000000000023")
	developerCredentials := &integrationCredentialProvider{credential: "developer-installation-token-secret"}
	reviewerCredentials := &integrationCredentialProvider{
		credential: "reviewer-installation-token-secret",
		err: &githubapi.NotInstalledError{
			Owner: "acme", Repository: "widgets", Cause: errors.New("reviewer-installation-token-secret"),
		},
	}
	preparer := &integrationTurnPreparer{}
	worker, err := agentturn.NewWorker(database, developerCredentials, reviewerCredentials, preparer, agentturn.WorkerConfig{
		ClaimOwner: "preparation-worker", LeaseDuration: 5 * time.Second,
		HeartbeatInterval: time.Second, IdlePollInterval: time.Millisecond, RetryDelay: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	processed, err := worker.ProcessNext(ctx)
	metadata := githubapi.ExtractSafeErrorMetadata(err)
	var notInstalled *githubapi.NotInstalledError
	if !processed || err == nil || !metadata.Permanent || errors.As(err, &notInstalled) {
		t.Fatalf("ProcessNext() = (%t, %v), metadata %#v, raw NotInstalled reachable %t; want safely classified missing Reviewer installation",
			processed, err, metadata, notInstalled != nil)
	}
	if developerCredentials.calls != 1 || reviewerCredentials.calls != 1 || preparer.called {
		t.Fatalf("preparation dependencies = Developer calls %d, Reviewer calls %d, Preparer called %t",
			developerCredentials.calls, reviewerCredentials.calls, preparer.called)
	}
	var workflowStatus, desiredAssignmentStatus, workflowReason, attemptReason, preparationStatus, preparationError string
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.desired_assignment_status, workflow.human_handoff_reason,
       attempt.human_handoff_reason, preparation.status, preparation.last_error
FROM workflows AS workflow
JOIN workflow_attempts AS attempt ON attempt.id = $2
JOIN jobs AS preparation ON preparation.workflow_id = workflow.id AND preparation.kind = 'PREPARE_AGENT_TURN'
WHERE workflow.id = $1`, application.WorkflowID, "62000000-0000-4000-8000-000000000023").Scan(
		&workflowStatus, &desiredAssignmentStatus, &workflowReason, &attemptReason, &preparationStatus, &preparationError,
	); err != nil {
		t.Fatal(err)
	}
	if workflowStatus != string(workflow.StateNeedsHuman) || desiredAssignmentStatus != string(workflow.AssignmentWaitingForHuman) ||
		workflowReason != string(workflow.ReasonAgentTurnPreparationFailed) || attemptReason != string(workflow.ReasonAgentTurnPreparationFailed) ||
		preparationStatus != string(store.JobFailed) {
		t.Errorf("missing Reviewer handoff = Workflow %s desired %s reasons %s/%s preparation %s",
			workflowStatus, desiredAssignmentStatus, workflowReason, attemptReason, preparationStatus)
	}
	if strings.Contains(preparationError, "installation-token-secret") || strings.Contains(err.Error(), "installation-token-secret") {
		t.Fatalf("missing Reviewer installation exposed credential: returned %v, durable %q", err, preparationError)
	}
	var credentialPersisted bool
	if err := pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1 FROM jobs
    WHERE workflow_id = $1
      AND (payload::text LIKE '%' || $2 || '%'
        OR payload::text LIKE '%' || $3 || '%'
        OR COALESCE(last_error, '') LIKE '%' || $2 || '%'
        OR COALESCE(last_error, '') LIKE '%' || $3 || '%')
)`, application.WorkflowID, developerCredentials.credential, reviewerCredentials.credential).Scan(&credentialPersisted); err != nil {
		t.Fatal(err)
	}
	if credentialPersisted {
		t.Fatal("Developer or Reviewer installation credential was persisted")
	}
	var turns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM agent_turns WHERE workflow_id = $1`, application.WorkflowID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if turns != 0 {
		t.Errorf("missing Reviewer installation created %d Agent Turns", turns)
	}
}

func TestInitialPreparationFailureRetriggersWithNewAssignments(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	application := triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000024", "62000000-0000-4000-8000-000000000024")
	failed := claimPreparationJob(t, database, ctx)
	if _, err := database.AcknowledgeAgentTurnPreparationFailure(ctx, failed, errors.New("Reviewer GitHub App is not installed"), false, time.Second); err != nil {
		t.Fatalf("AcknowledgeAgentTurnPreparationFailure() error = %v", err)
	}

	var assignmentCount int
	var workflowStatus, runtimeState string
	if err := pool.QueryRow(ctx, `
SELECT count(assignment.id), workflow.status, workflow.desired_runtime_state
FROM workflows AS workflow
LEFT JOIN agent_assignments AS assignment ON assignment.workflow_id = workflow.id
WHERE workflow.id = $1
GROUP BY workflow.id`, application.WorkflowID).Scan(&assignmentCount, &workflowStatus, &runtimeState); err != nil {
		t.Fatal(err)
	}
	if assignmentCount != 0 || workflowStatus != string(workflow.StateNeedsHuman) || runtimeState != string(workflow.RuntimeStateCollected) {
		t.Fatalf("failed initial preparation = %d Assignments, Workflow %s with runtime %s; want 0, NEEDS_HUMAN, COLLECTED",
			assignmentCount, workflowStatus, runtimeState)
	}

	triggerPreparationWorkflow(t, database, ctx, "61000000-0000-4000-8000-000000000025", "62000000-0000-4000-8000-000000000025")
	prepared := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "recovered-profile", "openai/recovered")
	if prepared.Assignment.Role != workflow.RoleDeveloper || prepared.Assignment.Generation != 1 || prepared.Turn.Purpose != workflow.TurnPurposeReactivation {
		t.Fatalf("retriggered preparation = %#v, want first-generation Developer reactivation", prepared)
	}
	assignments, err := database.ListAgentAssignments(ctx, application.WorkflowID)
	if err != nil || len(assignments) != 2 {
		t.Fatalf("retriggered Assignments = (%#v, %v), want Developer and Reviewer", assignments, err)
	}
}

type integrationCredentialProvider struct {
	credential string
	err        error
	calls      int
}

func (provider *integrationCredentialProvider) RepositoryCredential(context.Context, string, string) (string, error) {
	provider.calls++
	return provider.credential, provider.err
}

type integrationTurnPreparer struct {
	called bool
}

type integrationTurnPreparerFunc func(context.Context, agentturn.Request) (agentturn.Result, error)

func (prepare integrationTurnPreparerFunc) Prepare(ctx context.Context, request agentturn.Request) (agentturn.Result, error) {
	return prepare(ctx, request)
}

func (preparer *integrationTurnPreparer) Prepare(context.Context, agentturn.Request) (agentturn.Result, error) {
	preparer.called = true
	return agentturn.Result{}, nil
}

func prepareTurn(t *testing.T, database *store.Store, ctx context.Context, lease store.JobLease, commitSHA, model string) store.AgentTurnPreparationCommit {
	t.Helper()
	prepared, err := database.PrepareAgentTurn(ctx, lease, preparationSpec(commitSHA, model))
	if err != nil {
		t.Fatalf("PrepareAgentTurn() error = %v", err)
	}
	return prepared
}

func preparationSpec(commitSHA, developerModel string) store.AgentTurnPreparationSpec {
	bindings := preparationBindings()
	developerHash := sha256.Sum256([]byte(commitSHA))
	reviewerHash := sha256.Sum256([]byte(commitSHA + "-reviewer"))
	spec := store.AgentTurnPreparationSpec{
		Developer: store.RolePreparation{
			Binding: bindings[workflow.RoleDeveloper],
			Profile: store.AgentProfileSnapshot{
				CommitSHA: commitSHA, ContentSHA256: developerHash[:],
				Config: agentProfileConfig("developer", workflow.RoleDeveloper, "opencode-acp/1", developerModel, "", 40, "Perform development.", nil),
			},
		},
		Reviewer: store.RolePreparation{
			Binding: bindings[workflow.RoleReviewer],
			Profile: store.AgentProfileSnapshot{
				CommitSHA: commitSHA + "-reviewer", ContentSHA256: reviewerHash[:],
				Config: agentProfileConfig("reviewer", workflow.RoleReviewer, "opencode-acp/1", "anthropic/reviewer", "", 40, "Perform review.", nil),
			},
		},
	}
	return spec
}

func acquireAndBindTurn(t *testing.T, database *store.Store, ctx context.Context, prepared store.AgentTurnPreparationCommit, acpSessionID string) store.AgentTurnLease {
	t.Helper()
	job := claimAgentTurnJob(t, database, ctx, prepared.Turn, time.Second)
	lease, err := database.AcquireAgentTurn(ctx, job, prepared.Turn.ControlRevision, "runtime", time.Second, 1)
	if err != nil {
		t.Fatalf("AcquireAgentTurn() error = %v", err)
	}
	if _, err := database.BindAgentSessionACP(ctx, lease, acpSessionID, json.RawMessage(`{"resume":true}`)); err != nil {
		t.Fatalf("BindAgentSessionACP() error = %v", err)
	}
	return lease
}

func prepareHumanControlledSession(t *testing.T, database *store.Store, ctx context.Context, deliveryID, eventID string) store.AgentSession {
	t.Helper()
	triggerPreparationWorkflow(t, database, ctx, deliveryID, eventID)
	prepared := prepareTurn(t, database, ctx, claimPreparationJob(t, database, ctx), "human-profile", "openai/human")
	lease := acquireAndBindTurn(t, database, ctx, prepared, "human-acp")
	settleAcquiredTurn(t, database, ctx, lease, store.AgentTurnSucceeded)
	active, err := database.GetAgentSession(ctx, prepared.Session.ID)
	if err != nil {
		t.Fatalf("GetAgentSession() before human transfer error = %v", err)
	}
	human, err := database.TransferAgentSessionControl(ctx, active.ID, active.ControlRevision, store.SessionControlHuman, "human-controller")
	if err != nil {
		t.Fatalf("TransferAgentSessionControl() to human error = %v", err)
	}
	return human
}

func settleAcquiredTurn(t *testing.T, database *store.Store, ctx context.Context, lease store.AgentTurnLease, status store.AgentTurnStatus) {
	t.Helper()
	if err := database.OpenMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("OpenMutationAdmission() error = %v", err)
	}
	if err := database.CloseMutationAdmission(ctx, lease); err != nil {
		t.Fatalf("CloseMutationAdmission() error = %v", err)
	}
	completion := store.AgentTurnCompletion{Status: status}
	if status == store.AgentTurnSucceeded {
		completion.Outcome = json.RawMessage(`{"ok":true}`)
	} else {
		completion.LastError = "test failure"
	}
	if err := database.FinalizeAgentTurn(ctx, lease, completion); err != nil {
		t.Fatalf("FinalizeAgentTurn() error = %v", err)
	}
}

func preparationBindings() map[workflow.Role]store.AssignmentRuntimeBinding {
	return map[workflow.Role]store.AssignmentRuntimeBinding{
		workflow.RoleDeveloper: {
			AgentProfileName: "developer", RuntimeProfileName: "opencode-acp", RuntimeProfileVersion: "1",
			RuntimeProfileContentSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RuntimeImageDigest:          "sha256:developer-runtime",
		},
		workflow.RoleReviewer: {
			AgentProfileName: "reviewer", RuntimeProfileName: "opencode-acp", RuntimeProfileVersion: "1",
			RuntimeProfileContentSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			RuntimeImageDigest:          "sha256:reviewer-runtime",
		},
	}
}

func agentProfileConfig(name string, role workflow.Role, runtime, model, variant string, steps int, instructions string, permissions map[string]string) json.RawMessage {
	if permissions == nil {
		permissions = map[string]string{"edit": "deny", "read": "allow"}
	}
	profile := map[string]any{
		"name": name, "path": ".omnigrex/team/" + name + ".md", "role": role,
		"runtime": runtime, "model": model, "steps": steps,
		"permissions": permissions, "instructions": instructions,
	}
	if variant != "" {
		profile["variant"] = variant
	}
	encoded, _ := json.Marshal(profile)
	return encoded
}

func triggerPreparationWorkflow(t *testing.T, database *store.Store, ctx context.Context, deliveryID, attemptID string) store.WorkflowApplication {
	t.Helper()
	claim := claimWorkflowDelivery(t, database, ctx, workflowDelivery(deliveryID))
	application, err := database.CompleteWebhookTransition(ctx, claim.DeliveryID, claim.ClaimToken,
		normalizedPayload(claim.DeliveryID, "trigger"), workflowLocator(), func(snapshot workflow.Snapshot) workflow.Decision {
			return workflow.Reduce(snapshot, workflow.TriggerEvent{
				EventMetadata: workflow.EventMetadata{
					ID: claim.DeliveryID, ObservedAt: time.Date(2026, time.September, 3, 10, 0, 0, 0, time.UTC),
					WorkItem: workflow.WorkItem{RepositoryID: 9123, IssueID: 456, IssueNumber: 12}, ExpectedRevision: snapshot.Revision,
				},
				AttemptID: attemptID, AttemptNumber: snapshot.LastAttemptNumber + 1,
			})
		})
	if err != nil {
		t.Fatalf("CompleteWebhookTransition() error = %v", err)
	}
	return application
}

func claimPreparationJob(t *testing.T, database *store.Store, ctx context.Context) store.JobLease {
	t.Helper()
	lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.PrepareAgentTurnJobKind, "preparation-worker", 5*time.Second)
	if err != nil || lease == nil {
		t.Fatalf("ClaimJobKind() = (%#v, %v), want preparation job", lease, err)
	}
	return *lease
}

func insertPreparationJob(t *testing.T, pool *pgxpool.Pool, workflowID, attemptID string, revision int64, mode workflow.AssignmentGeneration, role workflow.Role, purpose workflow.TurnPurpose, expectedHeadSHA, retryOfTurnID string) {
	t.Helper()
	jobID := fmt.Sprintf("64000000-0000-4000-8000-%012d", revision)
	payload, err := json.Marshal(map[string]any{
		"mode": mode, "role": role, "purpose": purpose, "expected_head_sha": expectedHeadSHA,
		"retry_of_turn_id": retryOfTurnID, "revision": revision,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO jobs (
    id, queue, kind, payload, status, priority, available_at, max_attempts,
    idempotency_key, workflow_id, workflow_attempt_id
)
VALUES ($1, $2, $3, $4, 'AVAILABLE', 0, clock_timestamp(), 3, $5, $6, $7)`,
		jobID, store.WorkflowActionQueue, store.PrepareAgentTurnJobKind, payload,
		fmt.Sprintf("test-preparation:%s:%d", workflowID, revision), workflowID, attemptID); err != nil {
		t.Fatalf("insert preparation Job: %v", err)
	}
}

func setWorkflowRevision(t *testing.T, pool *pgxpool.Pool, workflowID string, revision int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE workflows SET state_revision = $2 WHERE id = $1`, workflowID, revision); err != nil {
		t.Fatalf("update Workflow revision: %v", err)
	}
}

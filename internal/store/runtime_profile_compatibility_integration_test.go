//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/store/migrations"
	"github.com/jozala/omnigrex/internal/workflow"
)

func TestRuntimeProfileCompatibilityResultRecordIsIdempotentAndConflictDetecting(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	source := compatibilityProfile(t, "a", "amd64")
	target := compatibilityProfile(t, "b", "amd64")
	result := runtimeprofile.NewCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform,
		time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))

	if err := database.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
		t.Fatalf("RecordRuntimeProfileCompatibilityResult() error = %v", err)
	}
	if err := database.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
		t.Fatalf("idempotent RecordRuntimeProfileCompatibilityResult() error = %v", err)
	}
	file := runtimeprofile.CompatibilityResultsFile{
		SchemaVersion: runtimeprofile.CompatibilityResultsSchemaVersion,
		Results:       []runtimeprofile.CompatibilityResult{result},
	}
	if err := database.ImportRuntimeProfileCompatibilityResults(ctx, file); err != nil {
		t.Fatalf("idempotent ImportRuntimeProfileCompatibilityResults() error = %v", err)
	}
	conflict := result
	conflict.QualifiedAt = "2026-09-06T12:00:01Z"
	if err := database.RecordRuntimeProfileCompatibilityResult(ctx, conflict); !errors.Is(err, store.ErrRuntimeProfileCompatibilityResultConflict) {
		t.Fatalf("conflicting RecordRuntimeProfileCompatibilityResult() error = %v", err)
	}
	var count int
	var qualifiedAt time.Time
	if err := pool.QueryRow(ctx, `SELECT count(*), min(qualified_at) FROM runtime_profile_compatibility_results`).Scan(&count, &qualifiedAt); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !qualifiedAt.Equal(result.QualifiedTime()) {
		t.Fatalf("recorded compatibility results = %d at %s", count, qualifiedAt)
	}
	if _, err := pool.Exec(ctx, `UPDATE runtime_profile_compatibility_results SET qualified_at = qualified_at + interval '1 second'`); err == nil {
		t.Fatal("database UPDATE of immutable compatibility result succeeded")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM runtime_profile_compatibility_results`); err == nil {
		t.Fatal("database DELETE of immutable compatibility result succeeded")
	}
}

func TestRuntimeProfileCompatibilityQualificationRequiresEveryExactAxis(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(*runtimeprofile.CompatibilityResult)
	}{
		{name: "source name", mutate: func(result *runtimeprofile.CompatibilityResult) { result.Source.Name = "other-runtime" }},
		{name: "source version", mutate: func(result *runtimeprofile.CompatibilityResult) { result.Source.Version = "v2" }},
		{name: "source content hash", mutate: func(result *runtimeprofile.CompatibilityResult) {
			result.Source.ContentSHA256 = strings.Repeat("c", 64)
		}},
		{name: "source image digest", mutate: func(result *runtimeprofile.CompatibilityResult) { result.Source.Image = compatibilityImage("c") }},
		{name: "target name", mutate: func(result *runtimeprofile.CompatibilityResult) { result.Target.Name = "other-runtime" }},
		{name: "target version", mutate: func(result *runtimeprofile.CompatibilityResult) { result.Target.Version = "v2" }},
		{name: "target content hash", mutate: func(result *runtimeprofile.CompatibilityResult) {
			result.Target.ContentSHA256 = strings.Repeat("c", 64)
		}},
		{name: "target image digest", mutate: func(result *runtimeprofile.CompatibilityResult) { result.Target.Image = compatibilityImage("c") }},
		{name: "platform", mutate: func(result *runtimeprofile.CompatibilityResult) { result.Platform.Arch = "arm64" }},
	}
	for index, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			source, target, _, _ := prepareCollectedCompatibilityGeneration(t, database, pool, ctx, index+100)
			result := runtimeprofile.NewCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform,
				time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
			test.mutate(&result)
			if err := database.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
				t.Fatalf("record mismatched result: %v", err)
			}
			if err := database.CheckPendingRuntimeProfileCompatibility(ctx, target); !errors.Is(err, store.ErrRuntimeProfileCompatibilityQualificationMissing) {
				t.Fatalf("CheckPendingRuntimeProfileCompatibility() error = %v", err)
			}
		})
	}
}

func TestCollectedRuntimeProfileCandidateRequiresQualificationBeforeNewGeneration(t *testing.T) {
	t.Run("missing creates configuration Human Handoff without partial generation", func(t *testing.T) {
		databases, pool := openPhaseFiveStores(t, 1)
		database := databases[0]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, target, workflowID, lease := prepareCollectedCompatibilityGeneration(t, database, pool, ctx, 210)
		if err := database.CheckPendingRuntimeProfileCompatibility(ctx, target); !errors.Is(err, store.ErrRuntimeProfileCompatibilityQualificationMissing) {
			t.Fatalf("readiness compatibility check error = %v", err)
		}
		candidate := compatibilityPreparationSpec("candidate-missing", target)
		if _, err := database.PrepareAgentTurn(ctx, lease, candidate); !errors.Is(err, store.ErrAssignmentConfigurationConflict) ||
			!errors.Is(err, store.ErrRuntimeProfileCompatibilityQualificationMissing) {
			t.Fatalf("PrepareAgentTurn() error = %v, want missing qualification configuration conflict", err)
		}
		assertNoCompatibilityGenerationSideEffects(t, pool, ctx, workflowID)
		if _, err := database.AcknowledgeAssignmentConfigurationConflict(ctx, lease, candidate); err != nil {
			t.Fatalf("AcknowledgeAssignmentConfigurationConflict() error = %v", err)
		}
		assertNoCompatibilityGenerationSideEffects(t, pool, ctx, workflowID)
		var workflowStatus, handoffReason, jobStatus string
		if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.human_handoff_reason, job.status
FROM workflows AS workflow JOIN jobs AS job ON job.id = $2
WHERE workflow.id = $1`, workflowID, lease.ID).Scan(&workflowStatus, &handoffReason, &jobStatus); err != nil {
			t.Fatal(err)
		}
		if workflowStatus != "NEEDS_HUMAN" || handoffReason != string(workflow.ReasonAssignmentConfigurationConflict) || jobStatus != "SUCCEEDED" {
			t.Fatalf("qualification handoff = Workflow %s reason %s, Job %s", workflowStatus, handoffReason, jobStatus)
		}
		if err := database.CheckPendingRuntimeProfileCompatibility(ctx, target); err != nil {
			t.Fatalf("readiness after Human Handoff error = %v", err)
		}
	})

	t.Run("qualified candidate creates new generation", func(t *testing.T) {
		databases, pool := openPhaseFiveStores(t, 1)
		database := databases[0]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		source, target, workflowID, lease := prepareCollectedCompatibilityGeneration(t, database, pool, ctx, 211)
		result := runtimeprofile.NewCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform,
			time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC))
		if err := database.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
			t.Fatal(err)
		}
		if err := database.CheckPendingRuntimeProfileCompatibility(ctx, target); err != nil {
			t.Fatalf("qualified readiness error = %v", err)
		}
		prepared, err := database.PrepareAgentTurn(ctx, lease, compatibilityPreparationSpec("candidate-qualified", target))
		if err != nil {
			t.Fatalf("qualified PrepareAgentTurn() error = %v", err)
		}
		if prepared.Assignment.Generation != 2 || prepared.Assignment.RuntimeImageDigest != target.Binding().Image ||
			prepared.Session.RuntimeImageDigest != target.Binding().Image || prepared.Session.Status != store.AgentSessionCreating {
			t.Fatalf("qualified new generation = %#v", prepared)
		}
		var assignments, sessions, turns int
		if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_assignments WHERE workflow_id = $1),
       (SELECT count(*) FROM agent_sessions AS session JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id WHERE assignment.workflow_id = $1),
       (SELECT count(*) FROM agent_turns WHERE workflow_id = $1)`, workflowID).Scan(&assignments, &sessions, &turns); err != nil {
			t.Fatal(err)
		}
		if assignments != 4 || sessions != 2 || turns != 2 {
			t.Fatalf("qualified durable generations = Assignments %d, Sessions %d, Turns %d", assignments, sessions, turns)
		}
	})
}

func TestNewAssignmentCandidateRequiresQualificationAgainstOtherWorkflowHistory(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	source := compatibilityProfile(t, "a", "amd64")
	target := compatibilityProfile(t, "b", "amd64")
	historical := seedAgentSession(t, pool, 212)
	setFixtureRuntimeBinding(t, pool, historical, source.Binding(), source.Binding())
	application := triggerPreparationWorkflow(t, database, ctx,
		"61000000-0000-4000-8000-000000000212", "62000000-0000-4000-8000-000000000212")
	lease := claimPreparationJob(t, database, ctx)
	candidate := compatibilityPreparationSpec("other-workflow-candidate", target)
	if _, err := database.PrepareAgentTurn(ctx, lease, candidate); !errors.Is(err, store.ErrRuntimeProfileCompatibilityQualificationMissing) {
		t.Fatalf("PrepareAgentTurn() without installation-wide qualification error = %v", err)
	}
	if assignments, err := database.ListAgentAssignments(ctx, application.WorkflowID); err != nil || len(assignments) != 0 {
		t.Fatalf("candidate Workflow Assignments before qualification = (%#v, %v)", assignments, err)
	}
	result := runtimeprofile.NewCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform,
		time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC))
	if err := database.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	prepared, err := database.PrepareAgentTurn(ctx, lease, candidate)
	if err != nil {
		t.Fatalf("PrepareAgentTurn() after installation-wide qualification error = %v", err)
	}
	oldSession, err := database.GetAgentSession(ctx, historical.sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Assignment.RuntimeImageDigest != target.Binding().Image || oldSession.RuntimeImageDigest != source.Binding().Image {
		t.Fatalf("Runtime Profile bindings after candidate use = new %q, old %q", prepared.Assignment.RuntimeImageDigest, oldSession.RuntimeImageDigest)
	}
}

func TestCurrentLegacyRuntimeBindingBlocksNewGenerationUntilExplicitlySuperseded(t *testing.T) {
	t.Run("unresolved blocks with no partial generation", func(t *testing.T) {
		databases, pool := openPhaseFiveStores(t, 1)
		database := databases[0]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		seedAgentSession(t, pool, 74)

		assertLegacyRuntimeBindingBlocksCandidate(t, database, pool, ctx,
			compatibilityProfile(t, "c", "amd64"), 220)
	})

	t.Run("explicit supersession permits candidate", func(t *testing.T) {
		databases, pool := openPhaseFiveStores(t, 1)
		database := databases[0]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		legacy := seedAgentSession(t, pool, 75)
		if _, err := pool.Exec(ctx, `UPDATE agent_assignments SET status = 'SUPERSEDED' WHERE id = $1`, legacy.assignmentID); err != nil {
			t.Fatalf("explicitly supersede legacy Assignment: %v", err)
		}
		target := compatibilityProfile(t, "c", "amd64")
		application := triggerPreparationWorkflow(t, database, ctx,
			"61000000-0000-4000-8000-000000000221", "62000000-0000-4000-8000-000000000221")
		prepared, err := database.PrepareAgentTurn(ctx, claimPreparationJob(t, database, ctx),
			compatibilityPreparationSpec("candidate-after-supersession", target))
		if err != nil {
			t.Fatalf("PrepareAgentTurn() after explicit legacy supersession error = %v", err)
		}
		if prepared.Assignment.WorkflowID != application.WorkflowID || prepared.Assignment.Generation != 1 ||
			prepared.Assignment.RuntimeProfileContentSHA256 != target.ContentSHA256() {
			t.Fatalf("candidate after explicit legacy supersession = %#v", prepared.Assignment)
		}
	})
}

func TestMigratedLegacyRuntimeBindingBlocksNewGenerationWithoutPreventingStoreStartup(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	applyRecordedLegacyMigrations(t, pool, ctx)
	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status)
VALUES ('6b000000-0000-4000-8000-000000000001', 8600, 'legacy', 'repo', 8600, 8600, 'ACTIVE');
INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ('6b000000-0000-4000-8000-000000000002',
        '6b000000-0000-4000-8000-000000000001', 1, 'ACTIVE');
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ('6b000000-0000-4000-8000-000000000003',
        '6b000000-0000-4000-8000-000000000001', 'DEVELOPER', 'ACTIVE',
        'developer', 'legacy-runtime', 'legacy-version', 'legacy:image', 'legacy/runtime-state');
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ('6b000000-0000-4000-8000-000000000004',
        '6b000000-0000-4000-8000-000000000003', 1, 'legacy-session',
        'legacy-runtime', 'legacy-version', 'legacy:image', 'legacy/runtime-state', 'ACTIVE')`); err != nil {
		t.Fatalf("seed pre-Phase-6 legacy Runtime Profile binding: %v", err)
	}
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatalf("open Store over legacy Runtime Profile binding: %v", err)
	}
	t.Cleanup(database.Close)
	if err := database.Ready(ctx); err != nil {
		t.Fatalf("migrated Store readiness error = %v", err)
	}
	bindings, err := database.ListProtectedRuntimeBindings(ctx)
	if err != nil || len(bindings) != 0 {
		t.Fatalf("migrated legacy protected bindings = (%#v, %v), want no guessed binding", bindings, err)
	}
	var assignmentHash, sessionHash *string
	if err := pool.QueryRow(ctx, `
SELECT assignment.runtime_profile_content_sha256, session.runtime_profile_content_sha256
FROM agent_assignments AS assignment
JOIN agent_sessions AS session ON session.agent_assignment_id = assignment.id
WHERE assignment.id = '6b000000-0000-4000-8000-000000000003'`).Scan(&assignmentHash, &sessionHash); err != nil {
		t.Fatal(err)
	}
	if assignmentHash != nil || sessionHash != nil {
		t.Fatalf("migration reconstructed legacy Runtime Profile binding = %v/%v", assignmentHash, sessionHash)
	}

	assertLegacyRuntimeBindingBlocksCandidate(t, database, pool, ctx,
		compatibilityProfile(t, "d", "amd64"), 222)
}

func TestRuntimeProfileCompatibilityResultPersistsAcrossStoreRestart(t *testing.T) {
	postgres := startPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatal(err)
	}
	source := compatibilityProfile(t, "d", "amd64")
	target := compatibilityProfile(t, "e", "amd64")
	result := runtimeprofile.NewCompatibilityResult(source.Binding(), target.Binding(), target.Contract().Platform,
		time.Date(2026, 9, 6, 13, 0, 0, 0, time.UTC))
	if err := first.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(second.Close)
	if err := second.RecordRuntimeProfileCompatibilityResult(ctx, result); err != nil {
		t.Fatalf("idempotent result after Store restart error = %v", err)
	}
}

func TestMigrationTwelveAppliesFromCurrentSchema(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
CREATE TABLE schema_migrations (
    version BIGINT PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL CHECK (name <> ''),
    checksum BYTEA NOT NULL CHECK (octet_length(checksum) = 32),
    applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
)`); err != nil {
		t.Fatal(err)
	}
	for version, migration := range []struct{ filename, name string }{
		{"000001_bootstrap.sql", "bootstrap"},
		{"000002_normalized_events.sql", "normalized_events"},
		{"000003_durable_jobs_and_turn_fencing.sql", "durable_jobs_and_turn_fencing"},
		{"000004_phase_five_acknowledgement_barriers.sql", "phase_five_acknowledgement_barriers"},
		{"000005_single_live_workflow_successor.sql", "single_live_workflow_successor"},
		{"000006_agent_turn_preparation.sql", "agent_turn_preparation"},
		{"000007_scoped_mutation_operations.sql", "scoped_mutation_operations"},
		{"000008_agent_turn_settlements.sql", "agent_turn_settlements"},
		{"000009_workflow_action_exhaustion.sql", "workflow_action_exhaustion"},
		{"000010_workflow_action_failure_barriers.sql", "workflow_action_failure_barriers"},
		{"000011_durable_closure_retention.sql", "durable_closure_retention"},
	} {
		contents, err := migrations.Files.ReadFile(migration.filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply current-schema migration %s: %v", migration.filename, err)
		}
		checksum := sha256.Sum256(contents)
		if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`, version+1, migration.name, checksum[:]); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrations.Run(ctx, pool); err != nil {
		t.Fatalf("migration 000012 from current schema error = %v", err)
	}
	var version, tables int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT max(version) FROM schema_migrations),
       (SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'runtime_profile_compatibility_results')`).Scan(&version, &tables); err != nil {
		t.Fatal(err)
	}
	if version != 12 || tables != 1 {
		t.Fatalf("migrated current schema = version %d, compatibility tables %d", version, tables)
	}
}

func prepareCollectedCompatibilityGeneration(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, sequence int) (runtimeprofile.Profile, runtimeprofile.Profile, string, store.JobLease) {
	t.Helper()
	deliveryID := compatibilityUUID(61, sequence)
	attemptID := compatibilityUUID(62, sequence)
	application := triggerPreparationWorkflow(t, database, ctx, deliveryID, attemptID)
	source := compatibilityProfile(t, "a", "amd64")
	target := compatibilityProfile(t, "b", "amd64")
	first, err := database.PrepareAgentTurn(ctx, claimPreparationJob(t, database, ctx), compatibilityPreparationSpec("compatibility-source", source))
	if err != nil {
		t.Fatalf("prepare source generation: %v", err)
	}
	lease := acquireAndBindTurn(t, database, ctx, first, "compatibility-source-session")
	settleAcquiredTurn(t, database, ctx, lease, store.AgentTurnSucceeded)
	if _, err := pool.Exec(ctx, `
UPDATE agent_sessions AS session
SET status = 'DELETED', state_deleted_at = clock_timestamp(), updated_at = clock_timestamp()
FROM agent_assignments AS assignment
WHERE assignment.id = session.agent_assignment_id AND assignment.workflow_id = $1`, application.WorkflowID); err != nil {
		t.Fatalf("collect source generation Session: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_assignments
SET status = 'COMPLETED', completed_at = clock_timestamp(), state_deleted_at = clock_timestamp(), updated_at = clock_timestamp()
WHERE workflow_id = $1`, application.WorkflowID); err != nil {
		t.Fatalf("collect source generation Assignments: %v", err)
	}
	setWorkflowRevision(t, pool, application.WorkflowID, 2)
	insertPreparationJob(t, pool, application.WorkflowID, first.Turn.WorkflowAttemptID, 2,
		workflow.AssignmentGenerationNew, workflow.RoleDeveloper, workflow.TurnPurposeRequestedChanges, "", "")
	var normalizedEventID string
	if err := pool.QueryRow(ctx, `
SELECT normalized_event_id::text
FROM jobs
WHERE workflow_id = $1 AND normalized_event_id IS NOT NULL
ORDER BY created_at
LIMIT 1`, application.WorkflowID).Scan(&normalizedEventID); err != nil {
		t.Fatalf("read preparation action provenance: %v", err)
	}
	if _, err := pool.Exec(ctx, `
UPDATE jobs
SET normalized_event_id = $2, action_key = 'compatibility-new-generation'
WHERE workflow_id = $1 AND kind = 'PREPARE_AGENT_TURN' AND payload->>'revision' = '2'`,
		application.WorkflowID, normalizedEventID); err != nil {
		t.Fatalf("set preparation action provenance: %v", err)
	}
	return source, target, application.WorkflowID, claimPreparationJob(t, database, ctx)
}

func compatibilityPreparationSpec(commitSHA string, profile runtimeprofile.Profile) store.AgentTurnPreparationSpec {
	binding := profile.Binding()
	requirement := store.RuntimeCompatibilityRequirement{
		Platform: profile.Contract().Platform, StateContractVersion: runtimeprofile.StateContractVersion,
		WorkspacePath: runtimeprofile.StableWorkspacePath,
	}
	developerHash := sha256.Sum256([]byte(commitSHA))
	reviewerHash := sha256.Sum256([]byte(commitSHA + "-reviewer"))
	return store.AgentTurnPreparationSpec{
		Developer: store.RolePreparation{
			Binding: store.AssignmentRuntimeBinding{
				AgentProfileName: "developer", RuntimeProfileName: binding.Name, RuntimeProfileVersion: binding.Version,
				RuntimeProfileContentSHA256: binding.ContentSHA256, RuntimeImageDigest: binding.Image,
			},
			RuntimeCompatibility: requirement,
			Profile: store.AgentProfileSnapshot{CommitSHA: commitSHA, ContentSHA256: developerHash[:],
				Config: agentProfileConfig("developer", workflow.RoleDeveloper, binding.Name+"/"+binding.Version, "openai/compatibility", "", 40, "Perform development.", nil)},
		},
		Reviewer: store.RolePreparation{
			Binding: store.AssignmentRuntimeBinding{
				AgentProfileName: "reviewer", RuntimeProfileName: binding.Name, RuntimeProfileVersion: binding.Version,
				RuntimeProfileContentSHA256: binding.ContentSHA256, RuntimeImageDigest: binding.Image,
			},
			RuntimeCompatibility: requirement,
			Profile: store.AgentProfileSnapshot{CommitSHA: commitSHA + "-reviewer", ContentSHA256: reviewerHash[:],
				Config: agentProfileConfig("reviewer", workflow.RoleReviewer, binding.Name+"/"+binding.Version, "anthropic/compatibility", "", 40, "Perform review.", nil)},
		},
	}
}

func compatibilityProfile(t *testing.T, digestDigit, architecture string) runtimeprofile.Profile {
	t.Helper()
	profile, err := runtimeprofile.NewOpenCodeV1(compatibilityImage(digestDigit), runtimeprofile.Platform{OS: "linux", Arch: architecture})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func compatibilityImage(digestDigit string) string {
	return "registry.example/omnigrex/opencode@sha256:" + strings.Repeat(digestDigit, 64)
}

func compatibilityUUID(prefix, sequence int) string {
	return fmt.Sprintf("%02d000000-0000-4000-8000-%012d", prefix, sequence)
}

func assertLegacyRuntimeBindingBlocksCandidate(t *testing.T, database *store.Store, pool *pgxpool.Pool, ctx context.Context, target runtimeprofile.Profile, sequence int) string {
	t.Helper()
	application := triggerPreparationWorkflow(t, database, ctx,
		compatibilityUUID(61, sequence), compatibilityUUID(62, sequence))
	lease := claimPreparationJob(t, database, ctx)
	candidate := compatibilityPreparationSpec("legacy-candidate", target)
	if err := database.CheckPendingRuntimeProfileCompatibility(ctx, target); !errors.Is(err, store.ErrRuntimeProfileCompatibilityQualificationMissing) {
		t.Fatalf("legacy readiness compatibility check error = %v", err)
	}
	if _, err := database.PrepareAgentTurn(ctx, lease, candidate); !errors.Is(err, store.ErrAssignmentConfigurationConflict) ||
		!errors.Is(err, store.ErrRuntimeProfileCompatibilityQualificationMissing) {
		t.Fatalf("PrepareAgentTurn() with unresolved legacy history error = %v", err)
	}
	assertNoNewGenerationRows(t, pool, ctx, application.WorkflowID)
	if _, err := database.AcknowledgeAssignmentConfigurationConflict(ctx, lease, candidate); err != nil {
		t.Fatalf("AcknowledgeAssignmentConfigurationConflict() for legacy history error = %v", err)
	}
	assertNoNewGenerationRows(t, pool, ctx, application.WorkflowID)
	var workflowStatus, reason, jobStatus string
	if err := pool.QueryRow(ctx, `
SELECT workflow.status, workflow.human_handoff_reason, job.status
FROM workflows AS workflow
JOIN jobs AS job ON job.id = $2
WHERE workflow.id = $1`, application.WorkflowID, lease.ID).Scan(&workflowStatus, &reason, &jobStatus); err != nil {
		t.Fatal(err)
	}
	if workflowStatus != "NEEDS_HUMAN" || reason != string(workflow.ReasonAssignmentConfigurationConflict) || jobStatus != "SUCCEEDED" {
		t.Fatalf("legacy compatibility handoff = Workflow %s reason %s, Job %s", workflowStatus, reason, jobStatus)
	}
	return application.WorkflowID
}

func assertNoNewGenerationRows(t *testing.T, pool *pgxpool.Pool, ctx context.Context, workflowID string) {
	t.Helper()
	var assignments, sessions, turns, runJobs int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_assignments WHERE workflow_id = $1),
       (SELECT count(*) FROM agent_sessions AS session
        JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
        WHERE assignment.workflow_id = $1),
       (SELECT count(*) FROM agent_turns WHERE workflow_id = $1),
       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'RUN_AGENT_TURN')`, workflowID).Scan(
		&assignments, &sessions, &turns, &runJobs); err != nil {
		t.Fatal(err)
	}
	if assignments != 0 || sessions != 0 || turns != 0 || runJobs != 0 {
		t.Fatalf("partial legacy candidate generation = Assignments %d, Sessions %d, Turns %d, execution Jobs %d",
			assignments, sessions, turns, runJobs)
	}
}

func assertNoCompatibilityGenerationSideEffects(t *testing.T, pool *pgxpool.Pool, ctx context.Context, workflowID string) {
	t.Helper()
	var assignments, sessions, turns, generationTwo, queuedRuns int
	if err := pool.QueryRow(ctx, `
SELECT (SELECT count(*) FROM agent_assignments WHERE workflow_id = $1),
       (SELECT count(*) FROM agent_sessions AS session JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id WHERE assignment.workflow_id = $1),
       (SELECT count(*) FROM agent_turns WHERE workflow_id = $1),
       (SELECT count(*) FROM agent_assignments WHERE workflow_id = $1 AND generation = 2),
       (SELECT count(*) FROM jobs WHERE workflow_id = $1 AND kind = 'RUN_AGENT_TURN' AND status = 'AVAILABLE')`, workflowID).Scan(
		&assignments, &sessions, &turns, &generationTwo, &queuedRuns); err != nil {
		t.Fatal(err)
	}
	if assignments != 2 || sessions != 1 || turns != 1 || generationTwo != 0 || queuedRuns != 0 {
		t.Fatalf("partial candidate generation = Assignments %d, Sessions %d, Turns %d, generation 2 %d, queued runs %d",
			assignments, sessions, turns, generationTwo, queuedRuns)
	}
}

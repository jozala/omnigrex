//go:build integration

package store_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/store"
	"github.com/jozala/omnigrex/internal/store/migrations"
)

func TestProtectedRuntimeBindingsRemainUntilCollectionFinalization(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	fixture := seedAgentSession(t, pool, 61)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	binding := protectedTestBinding("a", "1")
	setFixtureRuntimeBinding(t, pool, fixture, binding, binding)

	assertProtectedBindings(t, database, ctx, []runtimeprofile.Binding{binding})
	prepareClosableFixture(t, pool, fixture)
	observedAt := time.Now().UTC().Add(-2 * time.Hour)
	closeAndSettleWithoutTurn(t, database, ctx, fixture, 61,
		"61000000-0000-4000-8000-000000000611", "protected-closure", "protected-retention",
		observedAt, observedAt.Add(time.Hour))
	lease, err := database.ClaimJobKind(ctx, store.WorkflowActionQueue, store.CollectAssignmentsJobKind, "protected-collector", time.Minute)
	if err != nil || lease == nil {
		t.Fatalf("claim collection Job = (%#v, %v)", lease, err)
	}
	authorization, err := database.AuthorizeAssignmentCollection(ctx, *lease)
	if err != nil {
		t.Fatalf("AuthorizeAssignmentCollection() error = %v", err)
	}
	assertProtectedBindings(t, database, ctx, []runtimeprofile.Binding{binding})
	if _, err := database.FinalizeAssignmentCollection(ctx, *lease, authorization.Targets[:1]); !errors.Is(err, store.ErrAssignmentCollectionIncomplete) {
		t.Fatalf("partial FinalizeAssignmentCollection() error = %v", err)
	}
	assertProtectedBindings(t, database, ctx, []runtimeprofile.Binding{binding})

	if _, err := database.FinalizeAssignmentCollection(ctx, *lease, authorization.Targets); err != nil {
		t.Fatalf("FinalizeAssignmentCollection() error = %v", err)
	}
	assertProtectedBindings(t, database, ctx, nil)
}

func TestProtectedRuntimeBindingsIncludeDistinctActiveAndRetainedDigests(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	active := seedAgentSession(t, pool, 62)
	retained := seedAgentSession(t, pool, 63)
	activeBinding := protectedTestBinding("b", "2")
	retainedBinding := protectedTestBinding("c", "3")
	setFixtureRuntimeBinding(t, pool, active, activeBinding, activeBinding)
	setFixtureRuntimeBinding(t, pool, retained, retainedBinding, retainedBinding)
	if _, err := pool.Exec(ctx, `
UPDATE agent_assignments SET status = 'COMPLETED', completed_at = clock_timestamp(), retention_until = clock_timestamp() + interval '1 day'
WHERE id = $1`, retained.assignmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions SET status = 'RETAINED', retained_at = clock_timestamp() WHERE id = $1`, retained.sessionID); err != nil {
		t.Fatal(err)
	}

	assertProtectedBindings(t, database, ctx, []runtimeprofile.Binding{activeBinding, retainedBinding})
}

func TestProtectedRuntimeBindingsRejectInconsistentAssignmentAndSession(t *testing.T) {
	databases, pool := openPhaseFiveStores(t, 1)
	database := databases[0]
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fixture := seedAgentSession(t, pool, 64)
	assignment := protectedTestBinding("d", "4")
	session := protectedTestBinding("e", "5")
	setFixtureRuntimeBinding(t, pool, fixture, assignment, session)

	if _, err := database.ListProtectedRuntimeBindings(ctx); !errors.Is(err, store.ErrProtectedRuntimeConfigurationConflict) {
		t.Fatalf("ListProtectedRuntimeBindings() error = %v, want configuration conflict", err)
	}
}

func TestProtectedRuntimeBindingsRejectMutableTagsAndLocalImageIDs(t *testing.T) {
	for index, image := range []string{
		"registry.example/omnigrex/opencode:latest",
		"sha256:" + strings.Repeat("f", 64),
	} {
		t.Run(image[:6], func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			fixture := seedAgentSession(t, pool, 65+index)
			binding := protectedTestBinding("f", "6")
			binding.Image = image
			setFixtureRuntimeBinding(t, pool, fixture, binding, binding)
			if _, err := database.ListProtectedRuntimeBindings(ctx); !errors.Is(err, store.ErrProtectedRuntimeConfigurationConflict) {
				t.Fatalf("ListProtectedRuntimeBindings() error = %v, want configuration conflict", err)
			}
		})
	}
}

func TestProtectedRuntimeBindingsRejectMixedLegacyAndQualifiedRows(t *testing.T) {
	for index, qualifiedTarget := range []string{"assignment", "session"} {
		t.Run(qualifiedTarget, func(t *testing.T) {
			databases, pool := openPhaseFiveStores(t, 1)
			database := databases[0]
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			fixture := seedAgentSession(t, pool, 70+index)
			binding := protectedTestBinding("9", "8")
			query := `
UPDATE agent_assignments
SET runtime_profile_name = $2, runtime_profile_version = $3,
    runtime_profile_content_sha256 = $4, runtime_image_digest = $5
WHERE id = $1`
			id := fixture.assignmentID
			if qualifiedTarget == "session" {
				query = `
UPDATE agent_sessions
SET runtime_profile_name = $2, runtime_profile_version = $3,
    runtime_profile_content_sha256 = $4, runtime_image_digest = $5
WHERE id = $1`
				id = fixture.sessionID
			}
			if _, err := pool.Exec(ctx, query, id, binding.Name, binding.Version, binding.ContentSHA256, binding.Image); err != nil {
				t.Fatal(err)
			}
			if _, err := database.ListProtectedRuntimeBindings(ctx); !errors.Is(err, store.ErrProtectedRuntimeConfigurationConflict) {
				t.Fatalf("ListProtectedRuntimeBindings() mixed legacy/qualified error = %v, want configuration conflict", err)
			}
		})
	}
}

func TestProtectedRuntimeBindingsIgnoreHandedOffUnreconstructablePhaseSixRows(t *testing.T) {
	postgres := startPostgres(t)
	pool := openPool(t, postgres.databaseURL(true))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	applyRecordedLegacyMigrations(t, pool, ctx)

	const (
		workflowID = "6a000000-0000-4000-8000-000000000001"
		attemptID  = "6a000000-0000-4000-8000-000000000002"
		statePath  = "legacy/shared-unreconstructable-state"
	)
	if _, err := pool.Exec(ctx, `
INSERT INTO workflows (id, repository_id, repository_owner, repository_name, issue_id, issue_number, status)
VALUES ($1, 1, 'owner', 'repo', 1, 1, 'ACTIVE')`, workflowID); err != nil {
		t.Fatalf("seed pre-Phase-6 Workflow: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO workflow_attempts (id, workflow_id, attempt_number, status)
VALUES ($1, $2, 1, 'ACTIVE')`, attemptID, workflowID); err != nil {
		t.Fatalf("seed pre-Phase-6 Workflow Attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_assignments (
    id, workflow_id, role, status, agent_profile_name, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path
)
VALUES ('6a000000-0000-4000-8000-000000000011', $1, 'DEVELOPER', 'ACTIVE',
        'developer', 'legacy-runtime', 'legacy-version', 'legacy:image', $2),
       ('6a000000-0000-4000-8000-000000000012', $1, 'REVIEWER', 'ACTIVE',
        'reviewer', 'legacy-runtime', 'legacy-version', 'legacy:image', $2)`, workflowID, statePath); err != nil {
		t.Fatalf("seed pre-Phase-6 Assignments: %v", err)
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO agent_sessions (
    id, agent_assignment_id, session_number, acp_session_id, runtime_profile_name,
    runtime_profile_version, runtime_image_digest, runtime_state_path, status
)
VALUES ('6a000000-0000-4000-8000-000000000021',
        '6a000000-0000-4000-8000-000000000011', 1, 'legacy-developer-session',
        'legacy-runtime', 'legacy-version', 'legacy:image', $1, 'ACTIVE'),
       ('6a000000-0000-4000-8000-000000000022',
        '6a000000-0000-4000-8000-000000000012', 1, 'legacy-reviewer-session',
        'legacy-runtime', 'legacy-version', 'legacy:image', $1, 'ACTIVE')`, statePath); err != nil {
		t.Fatalf("seed pre-Phase-6 Agent Sessions: %v", err)
	}

	passwordFile := filepath.Join(t.TempDir(), "database-password")
	if err := os.WriteFile(passwordFile, []byte(postgresPassword), 0o600); err != nil {
		t.Fatalf("write database password: %v", err)
	}
	database, err := store.Open(ctx, postgres.databaseURL(false), passwordFile)
	if err != nil {
		t.Fatalf("open Store over pre-Phase-6 schema: %v", err)
	}
	t.Cleanup(database.Close)

	bindings, err := database.ListProtectedRuntimeBindings(ctx)
	if err != nil || len(bindings) != 0 {
		t.Fatalf("ListProtectedRuntimeBindings() = (%#v, %v), want no launchable legacy binding", bindings, err)
	}
	catalog, err := runtimeprofile.NewCatalog(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	images := &legacyAvailabilityImages{}
	checker, err := runtimeprofile.NewAvailabilityChecker(database, catalog, images)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckBindings(ctx); err != nil {
		t.Fatalf("startup CheckBindings() for handed-off legacy rows error = %v", err)
	}
	if err := checker.Check(ctx); err != nil {
		t.Fatalf("readiness Check() for handed-off legacy rows error = %v", err)
	}
	if images.calls != 0 {
		t.Fatalf("legacy image availability checks = %d, want 0", images.calls)
	}

	var workflowStatus, handoffReason, assignmentStatuses, sessionStatuses, assignmentPaths, sessionPaths string
	var missingAssignmentHashes, missingSessionHashes int
	if err := pool.QueryRow(ctx, `SELECT status, human_handoff_reason FROM workflows WHERE id = $1`, workflowID).Scan(&workflowStatus, &handoffReason); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT string_agg(status, ',' ORDER BY role),
       string_agg(runtime_state_path, ',' ORDER BY role),
       count(*) FILTER (WHERE runtime_profile_content_sha256 IS NULL)
FROM agent_assignments WHERE workflow_id = $1`, workflowID).Scan(&assignmentStatuses, &assignmentPaths, &missingAssignmentHashes); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
SELECT string_agg(session.status, ',' ORDER BY assignment.role),
       string_agg(session.runtime_state_path, ',' ORDER BY assignment.role),
       count(*) FILTER (WHERE session.runtime_profile_content_sha256 IS NULL)
FROM agent_sessions AS session
JOIN agent_assignments AS assignment ON assignment.id = session.agent_assignment_id
WHERE assignment.workflow_id = $1`, workflowID).Scan(&sessionStatuses, &sessionPaths, &missingSessionHashes); err != nil {
		t.Fatal(err)
	}
	if workflowStatus != "NEEDS_HUMAN" || handoffReason != "migration_duplicate_runtime_state_path" ||
		assignmentStatuses != "WAITING_FOR_HUMAN,WAITING_FOR_HUMAN" || sessionStatuses != "ACTIVE,ACTIVE" ||
		assignmentPaths != statePath+","+statePath || sessionPaths != statePath+","+statePath ||
		missingAssignmentHashes != 2 || missingSessionHashes != 2 {
		t.Fatalf("migrated legacy state changed: workflow=(%s,%s), assignments=(%s,%s,%d), sessions=(%s,%s,%d)",
			workflowStatus, handoffReason, assignmentStatuses, assignmentPaths, missingAssignmentHashes,
			sessionStatuses, sessionPaths, missingSessionHashes)
	}
}

func applyRecordedLegacyMigrations(t *testing.T, pool *pgxpool.Pool, ctx context.Context) {
	t.Helper()
	if _, err := pool.Exec(ctx, `
CREATE TABLE schema_migrations (
    version BIGINT PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL CHECK (name <> ''),
    checksum BYTEA NOT NULL CHECK (octet_length(checksum) = 32),
    applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
)`); err != nil {
		t.Fatal(err)
	}
	legacy := []struct {
		version  int
		name     string
		filename string
	}{
		{1, "bootstrap", "000001_bootstrap.sql"},
		{2, "normalized_events", "000002_normalized_events.sql"},
		{3, "durable_jobs_and_turn_fencing", "000003_durable_jobs_and_turn_fencing.sql"},
		{4, "phase_five_acknowledgement_barriers", "000004_phase_five_acknowledgement_barriers.sql"},
		{5, "single_live_workflow_successor", "000005_single_live_workflow_successor.sql"},
	}
	for _, migration := range legacy {
		contents, err := migrations.Files.ReadFile(migration.filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(contents)); err != nil {
			t.Fatalf("apply legacy migration %s: %v", migration.filename, err)
		}
		checksum := sha256.Sum256(contents)
		if _, err := pool.Exec(ctx, `
INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			migration.version, migration.name, checksum[:]); err != nil {
			t.Fatalf("record legacy migration %s: %v", migration.filename, err)
		}
	}
}

type legacyAvailabilityImages struct {
	calls int
}

func (images *legacyAvailabilityImages) Available(context.Context, string, runtimeprofile.Platform) error {
	images.calls++
	return nil
}

func protectedTestBinding(hashDigit, imageDigit string) runtimeprofile.Binding {
	return runtimeprofile.Binding{
		Name: "opencode-acp", Version: "v1", ContentSHA256: strings.Repeat(hashDigit, 64),
		Image: "registry.example/omnigrex/opencode@sha256:" + strings.Repeat(imageDigit, 64),
	}
}

func setFixtureRuntimeBinding(t *testing.T, pool *pgxpool.Pool, fixture agentFixture, assignment, session runtimeprofile.Binding) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `
UPDATE agent_assignments
SET runtime_profile_name = $2, runtime_profile_version = $3,
    runtime_profile_content_sha256 = $4, runtime_image_digest = $5
WHERE id = $1`, fixture.assignmentID, assignment.Name, assignment.Version,
		assignment.ContentSHA256, assignment.Image); err != nil {
		t.Fatalf("set fixture Assignment Runtime Profile binding: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_sessions
SET runtime_profile_name = $2, runtime_profile_version = $3,
    runtime_profile_content_sha256 = $4, runtime_image_digest = $5
WHERE id = $1`, fixture.sessionID, session.Name, session.Version,
		session.ContentSHA256, session.Image); err != nil {
		t.Fatalf("set fixture Agent Session Runtime Profile binding: %v", err)
	}
}

func assertProtectedBindings(t *testing.T, database *store.Store, ctx context.Context, want []runtimeprofile.Binding) {
	t.Helper()
	got, err := database.ListProtectedRuntimeBindings(ctx)
	if err != nil {
		t.Fatalf("ListProtectedRuntimeBindings() error = %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("ListProtectedRuntimeBindings() = %#v, want %#v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("ListProtectedRuntimeBindings()[%d] = %#v, want %#v", index, got[index], want[index])
		}
	}
}

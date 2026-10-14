package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/workflow"
)

var ErrProtectedRuntimeConfigurationConflict = errors.New("protected Runtime Profile configuration conflict")

// AgentTurnPreparationRuntimeBindings identifies whether preparation must use current configuration
// or exact bindings already owned by an existing generation.
type AgentTurnPreparationRuntimeBindings struct {
	Mode        workflow.AssignmentGeneration
	Stage       workflow.StageID
	Role        workflow.Role
	Participant *ParticipantRuntimeBinding
	Developer   *AssignmentRuntimeBinding
	Reviewer    *AssignmentRuntimeBinding
}

// ListProtectedRuntimeBindings returns distinct launchable immutable bindings for every generation
// whose opaque state has not been marked deleted by assignment collection finalization.
func (store *Store) ListProtectedRuntimeBindings(ctx context.Context) ([]runtimeprofile.Binding, error) {
	return listProtectedRuntimeBindings(ctx, store.pool)
}

// ListProtectedRuntimeBindings returns protected bindings through the read-only Store.
func (store *ReadOnlyStore) ListProtectedRuntimeBindings(ctx context.Context) ([]runtimeprofile.Binding, error) {
	return listProtectedRuntimeBindings(ctx, store.pool)
}

func listProtectedRuntimeBindings(ctx context.Context, pool *pgxpool.Pool) ([]runtimeprofile.Binding, error) {
	rows, err := pool.Query(ctx, `
SELECT id::text, status, runtime_profile_name, runtime_profile_version,
       runtime_profile_content_sha256, runtime_image_digest
FROM agent_assignments
WHERE state_deleted_at IS NULL
ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query protected Assignments: %w", err)
	}
	type protectedAssignment struct {
		binding runtimeprofile.Binding
		legacy  bool
	}
	assignments := make(map[string]protectedAssignment)
	protected := make(map[runtimeprofile.Binding]struct{})
	for rows.Next() {
		var id string
		var status AgentAssignmentStatus
		var binding runtimeprofile.Binding
		var contentSHA256 *string
		if err := rows.Scan(&id, &status, &binding.Name, &binding.Version, &contentSHA256, &binding.Image); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan protected Assignment: %w", err)
		}
		if contentSHA256 == nil {
			// Phase 6 deliberately left this marker when legacy profile content was unreconstructable.
			assignments[id] = protectedAssignment{legacy: true}
			continue
		}
		binding.ContentSHA256 = *contentSHA256
		if binding.Validate() != nil {
			rows.Close()
			return nil, fmt.Errorf("%w: invalid Assignment binding", ErrProtectedRuntimeConfigurationConflict)
		}
		assignments[id] = protectedAssignment{binding: binding}
		protected[binding] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate protected Assignments: %w", err)
	}
	rows.Close()

	rows, err = pool.Query(ctx, `
SELECT session.agent_assignment_id::text, session.status,
       session.runtime_profile_name, session.runtime_profile_version,
       session.runtime_profile_content_sha256, session.runtime_image_digest
FROM agent_sessions AS session
WHERE session.state_deleted_at IS NULL
ORDER BY session.id`)
	if err != nil {
		return nil, fmt.Errorf("query protected Agent Sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var assignmentID string
		var status AgentSessionStatus
		var binding runtimeprofile.Binding
		var contentSHA256 *string
		if err := rows.Scan(&assignmentID, &status, &binding.Name, &binding.Version, &contentSHA256, &binding.Image); err != nil {
			return nil, fmt.Errorf("scan protected Agent Session: %w", err)
		}
		assignment, ok := assignments[assignmentID]
		if !ok || assignment.legacy != (contentSHA256 == nil) {
			return nil, fmt.Errorf("%w: Assignment and Agent Session bindings differ", ErrProtectedRuntimeConfigurationConflict)
		}
		if assignment.legacy {
			continue
		}
		binding.ContentSHA256 = *contentSHA256
		if status == AgentSessionDeleted || binding.Validate() != nil || binding != assignment.binding {
			return nil, fmt.Errorf("%w: Assignment and Agent Session bindings differ", ErrProtectedRuntimeConfigurationConflict)
		}
		protected[binding] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate protected Agent Sessions: %w", err)
	}

	bindings := make([]runtimeprofile.Binding, 0, len(protected))
	for binding := range protected {
		bindings = append(bindings, binding)
	}
	sort.Slice(bindings, func(i, j int) bool {
		left, right := bindings[i], bindings[j]
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		if left.Version != right.Version {
			return left.Version < right.Version
		}
		if left.ContentSHA256 != right.ContentSHA256 {
			return left.ContentSHA256 < right.ContentSHA256
		}
		return left.Image < right.Image
	})
	return bindings, nil
}

// GetAgentTurnPreparationRuntimeBindings returns persisted bindings for an existing generation.
// A new generation deliberately has no persisted binding and therefore uses current configuration.
func (store *Store) GetAgentTurnPreparationRuntimeBindings(ctx context.Context, lease JobLease) (AgentTurnPreparationRuntimeBindings, error) {
	if !validUUID(lease.ID) || !validUUID(lease.LeaseToken) || lease.Attempt <= 0 {
		return AgentTurnPreparationRuntimeBindings{}, ErrAgentTurnPreparationFenceLost
	}
	var payloadBytes []byte
	err := store.pool.QueryRow(ctx, `
SELECT payload
FROM jobs
WHERE id = $1 AND kind = $2 AND status = 'LEASED' AND lease_token = $3
  AND attempt_count = $4`, lease.ID, PrepareAgentTurnJobKind, lease.LeaseToken, lease.Attempt).Scan(&payloadBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentTurnPreparationRuntimeBindings{}, ErrAgentTurnPreparationFenceLost
	}
	if err != nil {
		return AgentTurnPreparationRuntimeBindings{}, fmt.Errorf("read Agent Turn preparation mode: %w", err)
	}
	payload, err := decodeAgentTurnPreparationPayload(payloadBytes)
	if err != nil || !store.validAgentTurnPreparationPayload(payload, lease.Job) {
		return AgentTurnPreparationRuntimeBindings{}, ErrAgentTurnPreparationFenceLost
	}
	result := AgentTurnPreparationRuntimeBindings{Mode: payload.Mode, Stage: payload.Stage, Role: payload.Role}
	if payload.Mode == workflow.AssignmentGenerationNew {
		return result, nil
	}
	var binding ParticipantRuntimeBinding
	err = store.pool.QueryRow(ctx, `
SELECT participant.agent_profile_name, participant.runtime_profile_name,
       participant.runtime_profile_version, participant.runtime_profile_content_sha256,
       participant.runtime_image_digest
FROM stage_assignments AS stage_assignment
JOIN agent_assignments AS participant ON participant.id = stage_assignment.agent_participant_id
WHERE stage_assignment.workflow_id = $1
  AND stage_assignment.assignment_generation = (
      SELECT MAX(generation) FROM agent_assignments
      WHERE workflow_id = $1 AND state_deleted_at IS NULL
  )
  AND stage_assignment.stage_id = $2`, lease.WorkflowID, payload.Stage).Scan(
		&binding.AgentProfileName, &binding.RuntimeProfileName, &binding.RuntimeProfileVersion,
		&binding.RuntimeProfileContentSHA256, &binding.RuntimeImageDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return AgentTurnPreparationRuntimeBindings{}, fmt.Errorf("read Stage Participant Runtime binding: %w", err)
	}
	result.Participant = &binding
	switch payload.Role {
	case workflow.RoleDeveloper:
		result.Developer = (*AssignmentRuntimeBinding)(&binding)
	case workflow.RoleReviewer:
		result.Reviewer = (*AssignmentRuntimeBinding)(&binding)
	}
	return result, nil
}

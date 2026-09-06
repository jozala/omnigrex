package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
	"github.com/jozala/omnigrex/internal/workflow"
)

var ErrProtectedRuntimeConfigurationConflict = errors.New("protected Runtime Profile configuration conflict")

// AgentTurnPreparationRuntimeBindings identifies whether preparation must use current configuration
// or exact bindings already owned by an existing generation.
type AgentTurnPreparationRuntimeBindings struct {
	Mode      workflow.AssignmentGeneration
	Developer *AssignmentRuntimeBinding
	Reviewer  *AssignmentRuntimeBinding
}

// ListProtectedRuntimeBindings returns distinct launchable immutable bindings for every generation
// whose opaque state has not been marked deleted by assignment collection finalization.
func (store *Store) ListProtectedRuntimeBindings(ctx context.Context) ([]runtimeprofile.Binding, error) {
	rows, err := store.pool.Query(ctx, `
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
		if status == AgentAssignmentSuperseded || binding.Validate() != nil {
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

	rows, err = store.pool.Query(ctx, `
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
	if err != nil || !validAgentTurnPreparationPayload(payload, lease.Job) {
		return AgentTurnPreparationRuntimeBindings{}, ErrAgentTurnPreparationFenceLost
	}
	result := AgentTurnPreparationRuntimeBindings{Mode: payload.Mode}
	if payload.Mode == workflow.AssignmentGenerationNew {
		return result, nil
	}
	assignments, err := store.ListAgentAssignments(ctx, lease.WorkflowID)
	if err != nil {
		return AgentTurnPreparationRuntimeBindings{}, err
	}
	generation := 0
	for _, assignment := range assignments {
		if assignment.StateDeletedAt != nil || assignment.Status == AgentAssignmentSuperseded {
			continue
		}
		if generation != 0 && assignment.Generation != generation {
			return AgentTurnPreparationRuntimeBindings{}, ErrProtectedRuntimeConfigurationConflict
		}
		generation = assignment.Generation
		binding := assignment.AssignmentRuntimeBinding
		switch assignment.Role {
		case workflow.RoleDeveloper:
			result.Developer = &binding
		case workflow.RoleReviewer:
			result.Reviewer = &binding
		}
	}
	if result.Developer == nil || result.Reviewer == nil {
		return AgentTurnPreparationRuntimeBindings{}, ErrAgentTurnPreparationFenceLost
	}
	return result, nil
}

type runtimeProfileCompatibilityQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func runtimeProfileCompatibilityMissing(ctx context.Context, querier runtimeProfileCompatibilityQuerier, spec AgentTurnPreparationSpec) (bool, error) {
	targets := map[workflow.Role]RolePreparation{
		workflow.RoleDeveloper: spec.Developer,
		workflow.RoleReviewer:  spec.Reviewer,
	}
	// Qualified history remains relevant permanently. Unresolvable legacy history remains a barrier
	// only while its state can still be selected by a future Workflow Attempt.
	rows, err := querier.Query(ctx, `
SELECT role, runtime_profile_name, runtime_profile_version,
       runtime_profile_content_sha256, runtime_image_digest
FROM agent_assignments
WHERE runtime_profile_content_sha256 IS NOT NULL
   OR (status <> 'SUPERSEDED' AND state_deleted_at IS NULL)
ORDER BY workflow_id, generation, role, id`)
	if err != nil {
		return false, fmt.Errorf("query historical Runtime Profile bindings: %w", err)
	}
	type historicalBinding struct {
		role   workflow.Role
		source runtimeprofile.Binding
		legacy bool
	}
	history := make([]historicalBinding, 0)
	for rows.Next() {
		var role workflow.Role
		var source runtimeprofile.Binding
		var contentSHA256 *string
		if err := rows.Scan(&role, &source.Name, &source.Version, &contentSHA256, &source.Image); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan historical Runtime Profile binding: %w", err)
		}
		if contentSHA256 != nil {
			source.ContentSHA256 = *contentSHA256
		}
		history = append(history, historicalBinding{role: role, source: source, legacy: contentSHA256 == nil})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("iterate historical Runtime Profile bindings: %w", err)
	}
	rows.Close()
	for _, historical := range history {
		targetPreparation, ok := targets[historical.role]
		if !ok {
			return false, ErrProtectedRuntimeConfigurationConflict
		}
		source := historical.source
		target := runtimeprofile.Binding{
			Name:          targetPreparation.Binding.RuntimeProfileName,
			Version:       targetPreparation.Binding.RuntimeProfileVersion,
			ContentSHA256: targetPreparation.Binding.RuntimeProfileContentSHA256,
			Image:         targetPreparation.Binding.RuntimeImageDigest,
		}
		if source == target {
			continue
		}
		if historical.legacy || source.Validate() != nil || target.Validate() != nil ||
			!validRuntimeCompatibilityRequirement(targetPreparation.RuntimeCompatibility) {
			return true, nil
		}
		qualified, err := hasRuntimeProfileCompatibilityResult(ctx, querier, source, target, targetPreparation.RuntimeCompatibility)
		if err != nil {
			return false, err
		}
		if !qualified {
			return true, nil
		}
	}
	return false, nil
}

func runtimeProfileCompatibilityMissingForTarget(ctx context.Context, querier runtimeProfileCompatibilityQuerier, target runtimeprofile.Binding, requirement RuntimeCompatibilityRequirement) (bool, error) {
	// Keep this predicate aligned with preparation so readiness reports the same legacy barrier.
	rows, err := querier.Query(ctx, `
SELECT runtime_profile_name, runtime_profile_version,
       runtime_profile_content_sha256, runtime_image_digest
FROM agent_assignments
WHERE runtime_profile_content_sha256 IS NOT NULL
   OR (status <> 'SUPERSEDED' AND state_deleted_at IS NULL)
ORDER BY workflow_id, generation, role, id`)
	if err != nil {
		return false, fmt.Errorf("query historical Runtime Profile bindings for readiness: %w", err)
	}
	type historicalBinding struct {
		source runtimeprofile.Binding
		legacy bool
	}
	history := make([]historicalBinding, 0)
	for rows.Next() {
		var source runtimeprofile.Binding
		var contentSHA256 *string
		if err := rows.Scan(&source.Name, &source.Version, &contentSHA256, &source.Image); err != nil {
			rows.Close()
			return false, fmt.Errorf("scan historical Runtime Profile binding for readiness: %w", err)
		}
		if contentSHA256 != nil {
			source.ContentSHA256 = *contentSHA256
		}
		history = append(history, historicalBinding{source: source, legacy: contentSHA256 == nil})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, fmt.Errorf("iterate historical Runtime Profile bindings for readiness: %w", err)
	}
	rows.Close()
	for _, historical := range history {
		if historical.source == target {
			continue
		}
		if historical.legacy || historical.source.Validate() != nil {
			return true, nil
		}
		qualified, err := hasRuntimeProfileCompatibilityResult(ctx, querier, historical.source, target, requirement)
		if err != nil {
			return false, err
		}
		if !qualified {
			return true, nil
		}
	}
	return false, nil
}

func validRuntimeCompatibilityRequirement(requirement RuntimeCompatibilityRequirement) bool {
	return requirement.Platform.OS == "linux" &&
		(requirement.Platform.Arch == "amd64" || requirement.Platform.Arch == "arm64") &&
		requirement.StateContractVersion == runtimeprofile.StateContractVersion &&
		requirement.WorkspacePath == runtimeprofile.StableWorkspacePath
}

func hasRuntimeProfileCompatibilityResult(ctx context.Context, querier runtimeProfileCompatibilityQuerier, source, target runtimeprofile.Binding, requirement RuntimeCompatibilityRequirement) (bool, error) {
	var qualified bool
	err := querier.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM runtime_profile_compatibility_results
    WHERE source_runtime_profile_name = $1 AND source_runtime_profile_version = $2
      AND source_runtime_profile_content_sha256 = $3 AND source_runtime_image_digest = $4
      AND target_runtime_profile_name = $5 AND target_runtime_profile_version = $6
      AND target_runtime_profile_content_sha256 = $7 AND target_runtime_image_digest = $8
      AND platform_os = $9 AND platform_arch = $10
      AND state_contract_version = $11 AND workspace_path = $12
      AND qualification_suite = $13 AND qualification_version = $14
      AND outcome = 'success'
)`, source.Name, source.Version, source.ContentSHA256, source.Image,
		target.Name, target.Version, target.ContentSHA256, target.Image,
		requirement.Platform.OS, requirement.Platform.Arch,
		requirement.StateContractVersion, requirement.WorkspacePath,
		runtimeprofile.CompatibilityQualificationSuite,
		runtimeprofile.CompatibilityQualificationVersion).Scan(&qualified)
	if err != nil {
		return false, fmt.Errorf("check Runtime Profile compatibility qualification: %w", err)
	}
	return qualified, nil
}

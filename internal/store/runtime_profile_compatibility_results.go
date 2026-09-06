package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
)

var (
	ErrRuntimeProfileCompatibilityResultConflict       = errors.New("Runtime Profile compatibility result conflict")
	ErrRuntimeProfileCompatibilityQualificationMissing = errors.New("Runtime Profile compatibility qualification missing")
)

// RuntimeCompatibilityRequirement identifies the non-binding axes required by a candidate.
type RuntimeCompatibilityRequirement struct {
	Platform             runtimeprofile.Platform
	StateContractVersion string
	WorkspacePath        string
}

// RecordRuntimeProfileCompatibilityResult immutably records one successful qualification.
// An exact replay is idempotent; the same qualification key with different result content conflicts.
func (store *Store) RecordRuntimeProfileCompatibilityResult(ctx context.Context, result runtimeprofile.CompatibilityResult) error {
	if err := result.Validate(); err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin Runtime Profile compatibility result recording: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := recordRuntimeProfileCompatibilityResult(ctx, tx, result); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Runtime Profile compatibility result: %w", err)
	}
	return nil
}

// ImportRuntimeProfileCompatibilityResults atomically and idempotently imports a validated artifact.
func (store *Store) ImportRuntimeProfileCompatibilityResults(ctx context.Context, file runtimeprofile.CompatibilityResultsFile) error {
	if err := file.Validate(); err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin Runtime Profile compatibility result import: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, result := range file.Results {
		if err := recordRuntimeProfileCompatibilityResult(ctx, tx, result); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit Runtime Profile compatibility result import: %w", err)
	}
	return nil
}

func recordRuntimeProfileCompatibilityResult(ctx context.Context, tx pgx.Tx, result runtimeprofile.CompatibilityResult) error {
	digest, err := result.ContentSHA256()
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
INSERT INTO runtime_profile_compatibility_results (
    source_runtime_profile_name, source_runtime_profile_version,
    source_runtime_profile_content_sha256, source_runtime_image_digest,
    target_runtime_profile_name, target_runtime_profile_version,
    target_runtime_profile_content_sha256, target_runtime_image_digest,
    platform_os, platform_arch, state_contract_version, workspace_path,
    qualification_suite, qualification_version, qualified_at, outcome, result_sha256
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, 'success', $16)
ON CONFLICT DO NOTHING`,
		result.Source.Name, result.Source.Version, result.Source.ContentSHA256, result.Source.Image,
		result.Target.Name, result.Target.Version, result.Target.ContentSHA256, result.Target.Image,
		result.Platform.OS, result.Platform.Arch, result.StateContractVersion, result.WorkspacePath,
		result.QualificationSuite, result.QualificationVersion, result.QualifiedTime(), digest[:])
	if err != nil {
		return fmt.Errorf("record Runtime Profile compatibility result: %w", err)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var qualifiedAt time.Time
	var recordedDigest []byte
	err = tx.QueryRow(ctx, `
SELECT qualified_at, result_sha256
FROM runtime_profile_compatibility_results
WHERE source_runtime_profile_name = $1 AND source_runtime_profile_version = $2
  AND source_runtime_profile_content_sha256 = $3 AND source_runtime_image_digest = $4
  AND target_runtime_profile_name = $5 AND target_runtime_profile_version = $6
  AND target_runtime_profile_content_sha256 = $7 AND target_runtime_image_digest = $8
  AND platform_os = $9 AND platform_arch = $10 AND state_contract_version = $11
  AND workspace_path = $12 AND qualification_suite = $13 AND qualification_version = $14`,
		result.Source.Name, result.Source.Version, result.Source.ContentSHA256, result.Source.Image,
		result.Target.Name, result.Target.Version, result.Target.ContentSHA256, result.Target.Image,
		result.Platform.OS, result.Platform.Arch, result.StateContractVersion, result.WorkspacePath,
		result.QualificationSuite, result.QualificationVersion).Scan(&qualifiedAt, &recordedDigest)
	if err != nil {
		return fmt.Errorf("read existing Runtime Profile compatibility result: %w", err)
	}
	if !qualifiedAt.Equal(result.QualifiedTime()) || !bytes.Equal(recordedDigest, digest[:]) {
		return ErrRuntimeProfileCompatibilityResultConflict
	}
	return nil
}

// CheckPendingRuntimeProfileCompatibility reports pending new generations blocked by the current candidate.
func (store *Store) CheckPendingRuntimeProfileCompatibility(ctx context.Context, target runtimeprofile.Profile) error {
	contract := target.Contract()
	requirement := RuntimeCompatibilityRequirement{
		Platform: contract.Platform, StateContractVersion: runtimeprofile.StateContractVersion,
		WorkspacePath: runtimeprofile.StableWorkspacePath,
	}
	rows, err := store.pool.Query(ctx, `
SELECT DISTINCT workflow_id::text
FROM jobs
WHERE kind = 'PREPARE_AGENT_TURN' AND status IN ('AVAILABLE', 'LEASED')
  AND payload->>'mode' = 'NEW'
ORDER BY workflow_id`)
	if err != nil {
		return fmt.Errorf("query pending new Assignment generations: %w", err)
	}
	workflowIDs := make([]string, 0)
	for rows.Next() {
		var workflowID string
		if err := rows.Scan(&workflowID); err != nil {
			rows.Close()
			return fmt.Errorf("scan pending new Assignment generation: %w", err)
		}
		workflowIDs = append(workflowIDs, workflowID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate pending new Assignment generations: %w", err)
	}
	rows.Close()
	if len(workflowIDs) != 0 {
		missing, err := runtimeProfileCompatibilityMissingForTarget(ctx, store.pool, target.Binding(), requirement)
		if err != nil {
			return err
		}
		if missing {
			return fmt.Errorf("%w: pending new Assignment generation", ErrRuntimeProfileCompatibilityQualificationMissing)
		}
	}
	return nil
}

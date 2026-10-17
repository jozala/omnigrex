package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	runtimeprofile "github.com/jozala/omnigrex/internal/runtime/profile"
)

var (
	ErrRuntimeProfileCompatibilityResultConflict = errors.New("Runtime Profile compatibility result conflict")
)

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
	key, err := result.QualificationKey()
	if err != nil {
		return err
	}
	digest, err := result.ContentSHA256()
	if err != nil {
		return err
	}
	content, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO pi_runtime_profile_compatibility_results (qualification_key, result, result_sha256)
VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, key, content, digest[:])
	if err != nil {
		return err
	}
	var existing []byte
	if err := tx.QueryRow(ctx, `SELECT result_sha256 FROM pi_runtime_profile_compatibility_results WHERE qualification_key = $1`, key).Scan(&existing); err != nil {
		return err
	}
	if !bytes.Equal(existing, digest[:]) {
		return ErrRuntimeProfileCompatibilityResultConflict
	}
	return nil
}

// CheckPendingRuntimeProfileCompatibility validates the current candidate and
// rejects unsupported runtime history anywhere in the installation ledger.
func (store *Store) CheckPendingRuntimeProfileCompatibility(ctx context.Context, target runtimeprofile.Profile) error {
	return checkPendingRuntimeProfileCompatibility(ctx, store.pool, target)
}

// CheckPendingRuntimeProfileCompatibility checks installation history through the read-only Store.
func (store *ReadOnlyStore) CheckPendingRuntimeProfileCompatibility(ctx context.Context, target runtimeprofile.Profile) error {
	return checkPendingRuntimeProfileCompatibility(ctx, store.pool, target)
}

func checkPendingRuntimeProfileCompatibility(ctx context.Context, pool *pgxpool.Pool, target runtimeprofile.Profile) error {
	// New Participants receive the selected Pi image without migrating Sessions.
	// Retained Participants remain protected by exact binding/image/state checks.
	if target.Binding().Name != runtimeprofile.PiRuntimeName || target.Binding().Version != runtimeprofile.PiRuntimeVersion {
		return ErrProtectedRuntimeConfigurationConflict
	}
	if err := target.Binding().Validate(); err != nil {
		return err
	}
	var unsupported bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (
    SELECT 1 FROM agent_assignments
    WHERE runtime_profile_name <> $1 OR runtime_profile_version <> $2
    UNION ALL
    SELECT 1 FROM agent_sessions
    WHERE runtime_profile_name <> $1 OR runtime_profile_version <> $2
    UNION ALL
    SELECT 1 FROM runtime_profile_contracts
    WHERE contract->>'name' IS DISTINCT FROM $1
       OR contract->>'version' IS DISTINCT FROM $2
)`, runtimeprofile.PiRuntimeName, runtimeprofile.PiRuntimeVersion).Scan(&unsupported); err != nil {
		return fmt.Errorf("inspect installation Runtime Profile history: %w", err)
	}
	if unsupported {
		return fmt.Errorf("%w: installation contains unsupported Runtime Profile contracts", ErrProtectedRuntimeConfigurationConflict)
	}
	return nil
}

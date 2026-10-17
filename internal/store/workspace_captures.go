package store

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/runtime/execution"
)

var _ execution.CaptureLedger = (*Store)(nil)

// LoadCapture reads sealed facts independently of artifact availability or live
// authority. The unique operation identity cannot reinterpret its full request.
func (store *Store) LoadCapture(ctx context.Context, request execution.CaptureRequest) (execution.CaptureRef, bool, error) {
	encoded, err := encodeCaptureRequest(request)
	if err != nil {
		return execution.CaptureRef{}, false, err
	}
	return loadCapture(ctx, store.pool, request, encoded)
}

func loadCapture(ctx context.Context, queryer executionQueryer, request execution.CaptureRequest, encoded []byte) (execution.CaptureRef, bool, error) {
	var digest string
	var matches bool
	err := queryer.QueryRow(ctx, `
SELECT sha256, request = $5::jsonb
FROM workspace_captures
WHERE agent_turn_id = $1 AND execution_epoch = $2 AND purpose = $3 AND operation_id = $4`,
		request.Workspace.Key.AgentTurnID, request.Workspace.Key.ExecutionEpoch, request.Purpose, request.OperationID, encoded).Scan(&digest, &matches)
	if errors.Is(err, pgx.ErrNoRows) {
		return execution.CaptureRef{}, false, nil
	}
	if err != nil {
		return execution.CaptureRef{}, false, fmt.Errorf("load workspace capture: %w", err)
	}
	if !matches {
		return execution.CaptureRef{}, false, execution.ErrConflict
	}
	return execution.CaptureRef{Request: request, SHA256: digest}, true, nil
}

// SealCapture records facts after source admission and artifact sealing have
// completed. It grants no execution, publication, or settlement authority and
// must be called outside a Store fence callback, including with a one-slot pool.
func (store *Store) SealCapture(ctx context.Context, ref execution.CaptureRef) error {
	encoded, err := encodeCaptureRequest(ref.Request)
	if err != nil {
		return err
	}
	digest, err := hex.DecodeString(ref.SHA256)
	if err != nil || len(digest) != 32 || ref.SHA256 != strings.ToLower(ref.SHA256) {
		return execution.ErrInvalidSpec
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin workspace capture seal: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	stored, found, err := loadCapture(ctx, tx, ref.Request, encoded)
	if err != nil {
		return err
	}
	if found {
		if stored != ref {
			return execution.ErrConflict
		}
		return tx.Commit(ctx)
	}
	// Validate the actual durable lineage before inserting new facts. Shared row
	// locks retain that identity through commit without changing Workflow state.
	// A lease, mutation-admission state, or execution reservation is not evidence
	// of the durable Participant/Session/Turn/Runtime Profile binding.
	key := ref.Request.Workspace.Key
	var turnID string
	err = tx.QueryRow(ctx, `
SELECT turn.id::text
FROM agent_turns AS turn
JOIN agent_sessions AS session ON session.id = turn.agent_session_id
JOIN agent_assignments AS participant ON participant.id = session.agent_assignment_id
WHERE turn.id = $1 AND turn.execution_epoch = $2
  AND session.id = $3 AND participant.id = $4
  AND participant.runtime_profile_name = $5 AND participant.runtime_profile_version = $6
   AND session.runtime_profile_name = participant.runtime_profile_name
   AND session.runtime_profile_version = participant.runtime_profile_version
   AND session.runtime_profile_content_sha256 = participant.runtime_profile_content_sha256
   AND session.runtime_image_digest = participant.runtime_image_digest
   AND session.runtime_state_path = participant.runtime_state_path
FOR SHARE OF turn, session, participant`, key.AgentTurnID, key.ExecutionEpoch, key.AgentSessionID, key.AssignmentID,
		key.RuntimeProfileName, key.RuntimeProfileVersion).Scan(&turnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return execution.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("validate workspace capture identity: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO workspace_captures (agent_turn_id, execution_epoch, purpose, operation_id, request, sha256)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (agent_turn_id, execution_epoch, purpose, operation_id) DO NOTHING`,
		key.AgentTurnID, key.ExecutionEpoch, ref.Request.Purpose, ref.Request.OperationID, encoded, ref.SHA256); err != nil {
		return fmt.Errorf("seal workspace capture: %w", err)
	}
	// The unique index serializes racing first seals. Read again in this
	// transaction to compare against the winner without ever updating it.
	stored, found, err = loadCapture(ctx, tx, ref.Request, encoded)
	if err != nil {
		return err
	}
	if !found || stored != ref {
		return execution.ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit workspace capture seal: %w", err)
	}
	return nil
}

func encodeCaptureRequest(request execution.CaptureRequest) ([]byte, error) {
	key := request.Workspace.Key
	if !validUUID(key.AssignmentID) || !validUUID(key.AgentSessionID) || !validUUID(key.AgentTurnID) || key.ExecutionEpoch <= 0 ||
		key.RuntimeProfileName == "" || key.RuntimeProfileVersion == "" || request.OperationID == "" || len(request.OperationID) > 256 ||
		request.Purpose != execution.PublicationCapture && request.Purpose != execution.TerminalCapture {
		return nil, execution.ErrInvalidSpec
	}
	// JSON must preserve the supplied strings exactly, and PostgreSQL text/jsonb
	// cannot represent NUL. Do not silently replace invalid UTF-8 on marshal.
	for _, value := range []string{key.RuntimeProfileName, key.RuntimeProfileVersion, request.Workspace.Seed, request.OperationID} {
		if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return nil, execution.ErrInvalidSpec
		}
	}
	return json.Marshal(request)
}

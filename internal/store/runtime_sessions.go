package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/runtime/agent"
)

var ErrAgentSessionRuntimeConflict = errors.New("agent session runtime identity or capability conflict")

// ReserveAgentSessionRuntime records the exact Session identity before any
// external preparation. Retries after response loss use the same durable ID.
func (store *Store) ReserveAgentSessionRuntime(ctx context.Context, lease AgentTurnLease) (bound AgentSession, err error) {
	err = store.withLockedAgentTurnLeaseStatus(ctx, lease, "reserve runtime Agent Session", true, func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning {
			return ErrAgentTurnFenceLost
		}
		session, err := scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1 FOR UPDATE`, lease.AgentSessionID))
		if err != nil {
			return err
		}
		if session.RuntimeProfileName != "pi-rpc" || session.RuntimeProfileVersion != "v1" || session.Status != AgentSessionCreating && session.Status != AgentSessionActive {
			return ErrAgentSessionRuntimeConflict
		}
		if session.RuntimeSessionID == "" && session.Status == AgentSessionCreating {
			if _, err := tx.Exec(ctx, `UPDATE agent_sessions SET runtime_session_id = id::text, updated_at = clock_timestamp() WHERE id = $1`, session.ID); err != nil {
				return err
			}
		} else if session.RuntimeSessionID != session.ID {
			return ErrAgentSessionRuntimeConflict
		}
		bound, err = scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1`, session.ID))
		return err
	})
	return bound, err
}

// BindAgentSessionRuntime activates only the reserved exact identity under the
// still-live fence. Capability content is a narrow credential-free contract.
func (store *Store) BindAgentSessionRuntime(ctx context.Context, lease AgentTurnLease, ready agent.Ready) (bound AgentSession, err error) {
	canonical, err := canonicalJSON(ready.Capabilities)
	if err != nil {
		return bound, ErrAgentSessionRuntimeConflict
	}
	var capabilities struct {
		HistoryReplay       bool   `json:"history_replay"`
		SessionContinuation bool   `json:"session_continuation"`
		StateFormat         string `json:"state_format"`
	}
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&capabilities) != nil || !capabilities.HistoryReplay || !capabilities.SessionContinuation || capabilities.StateFormat != "pi-session/v3" {
		return bound, ErrAgentSessionRuntimeConflict
	}
	err = store.withLockedAgentTurnLeaseStatus(ctx, lease, "bind runtime Agent Session", true, func(tx pgx.Tx, turn lockedTurn) error {
		if turn.Status != AgentTurnRunning {
			return ErrAgentTurnFenceLost
		}
		session, err := scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1 FOR UPDATE`, lease.AgentSessionID))
		if err != nil {
			return err
		}
		if session.RuntimeProfileName != "pi-rpc" || session.RuntimeProfileVersion != "v1" || session.RuntimeSessionID == "" || session.RuntimeSessionID != ready.RuntimeSessionID {
			return ErrAgentSessionRuntimeConflict
		}
		if session.Status == AgentSessionCreating && len(session.RuntimeCapabilities) == 0 {
			if _, err := tx.Exec(ctx, `UPDATE agent_sessions SET runtime_capabilities = $2, status = 'ACTIVE', activated_at = COALESCE(activated_at, clock_timestamp()), updated_at = clock_timestamp() WHERE id = $1`, session.ID, canonical); err != nil {
				return err
			}
		} else if session.Status != AgentSessionActive || !bytes.Equal(session.RuntimeCapabilities, canonical) {
			return ErrAgentSessionRuntimeConflict
		}
		bound, err = scanAgentSession(tx.QueryRow(ctx, agentSessionSelect+` WHERE id = $1`, session.ID))
		return err
	})
	return bound, err
}

func (store *Store) ValidateRuntimeSessionPromptFence(ctx context.Context, lease AgentTurnLease, runtimeID string) error {
	return store.withLockedAgentTurnLease(ctx, lease, "validate runtime Session prompt fence", func(tx pgx.Tx, _ lockedTurn) error {
		var persisted string
		if err := tx.QueryRow(ctx, `SELECT COALESCE(runtime_session_id, '') FROM agent_sessions WHERE id = $1`, lease.AgentSessionID).Scan(&persisted); err != nil {
			return err
		}
		if persisted == "" || persisted != runtimeID {
			return ErrAgentSessionRuntimeConflict
		}
		return nil
	})
}

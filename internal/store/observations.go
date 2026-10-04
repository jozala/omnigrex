package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jozala/omnigrex/internal/telemetry"
)

//go:embed observations.sql
var durableObservationSQL string

// ObserveDurableState is a read-only, all-or-nothing aggregate snapshot. It never
// claims/reclaims work or returns payloads, credentials, or per-Workflow records.
func (store *Store) ObserveDurableState(ctx context.Context) (telemetry.DurableState, error) {
	ctx, cancel := context.WithTimeout(ctx, telemetry.ObservationTimeout)
	defer cancel()
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return telemetry.DurableState{}, fmt.Errorf("begin durable observation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Also bound server work if cancellation cannot be delivered promptly.
	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '4s'`); err != nil {
		return telemetry.DurableState{}, err
	}
	type stageObservation struct {
		Stage            string   `json:"stage"`
		Role             string   `json:"role"`
		State            string   `json:"state"`
		Purposes         []string `json:"purposes"`
		RequiresProposal bool     `json:"requires_proposal"`
	}
	var stages []stageObservation
	definition := store.reducer.Definition()
	for _, id := range definition.StageIDs() {
		stage, _ := definition.Stage(id)
		policy, _ := store.policies.Lookup(stage.Role)
		entry := stageObservation{Stage: string(id), Role: string(stage.Role), State: string(stage.State), RequiresProposal: policy.RequiresChangeProposal}
		for _, purpose := range stage.AcceptedPurposes {
			entry.Purposes = append(entry.Purposes, string(purpose))
		}
		stages = append(stages, entry)
	}
	stageJSON, err := json.Marshal(stages)
	if err != nil {
		return telemetry.DurableState{}, err
	}
	rows, err := tx.Query(ctx, durableObservationSQL, stageJSON)
	if err != nil {
		return telemetry.DurableState{}, fmt.Errorf("query durable observation: %w", err)
	}
	defer rows.Close()
	state := telemetry.DurableState{Workflows: map[string]int64{}, Pending: map[string]telemetry.PendingWork{}}
	for rows.Next() {
		var family, label string
		var count int64
		var age float64
		if err := rows.Scan(&family, &label, &count, &age); err != nil {
			return telemetry.DurableState{}, err
		}
		switch family {
		case "workflow":
			state.Workflows[label] = count
		case "active":
			state.ActiveTurns = count
		case "pending":
			state.Pending[label] = telemetry.PendingWork{Count: count, OldestSeconds: max(0, age)}
		case "mutation":
			state.UnresolvedMutations, state.OldestUnresolvedSeconds = count, max(0, age)
		}
	}
	if err := rows.Err(); err != nil {
		return telemetry.DurableState{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return telemetry.DurableState{}, err
	}
	return state, nil
}

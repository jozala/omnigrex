package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/runtime/profile"
)

func (store *Store) RuntimeProfileContract(ctx context.Context, binding profile.Binding) (json.RawMessage, error) {
	return runtimeProfileContract(ctx, store.pool, binding)
}

func (store *ReadOnlyStore) RuntimeProfileContract(ctx context.Context, binding profile.Binding) (json.RawMessage, error) {
	return runtimeProfileContract(ctx, store.pool, binding)
}

func runtimeProfileContract(ctx context.Context, pool *pgxpool.Pool, binding profile.Binding) (json.RawMessage, error) {
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	var content json.RawMessage
	err := pool.QueryRow(ctx, `SELECT contract FROM runtime_profile_contracts WHERE content_sha256 = $1`, binding.ContentSHA256).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, profile.ErrNotFound
	}
	return content, err
}

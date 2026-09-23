package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/role"
	"github.com/jozala/omnigrex/internal/store/migrations"
	"github.com/jozala/omnigrex/internal/workflow"
)

// Store owns the application's PostgreSQL connection pool.
type Store struct {
	pool     *pgxpool.Pool
	reducer  workflow.Reducer
	policies role.PolicyCatalog
}

// Config contains the deployment semantics required by a writable Store.
type Config struct {
	Reducer  workflow.Reducer
	Policies role.PolicyCatalog
}

// ReadOnlyStore owns a read-only PostgreSQL pool used for readiness and schema inspection.
type ReadOnlyStore struct {
	pool *pgxpool.Pool
}

// Open connects to PostgreSQL and installs the deployment's explicit Workflow and Role configuration.
func Open(ctx context.Context, databaseURL, passwordFile string, config Config) (*Store, error) {
	if !config.Reducer.Valid() {
		return nil, errors.New("open database store: invalid Workflow Reducer")
	}
	definition := config.Reducer.Definition()
	if !sameRoles(definition.Roles(), config.Policies.Roles()) {
		return nil, errors.New("open database store: Role policies do not match Workflow Definition")
	}
	if err := definition.ValidateRolePolicies(config.Policies); err != nil {
		return nil, fmt.Errorf("open database store: validate Role capabilities: %w", err)
	}
	pool, err := openPool(ctx, databaseURL, passwordFile, false)
	if err != nil {
		return nil, err
	}
	if err := migrations.Run(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return &Store{pool: pool, reducer: config.Reducer, policies: config.Policies}, nil
}

// OpenReadOnly connects to PostgreSQL without applying migrations and makes every connection read-only.
func OpenReadOnly(ctx context.Context, databaseURL, passwordFile string) (*ReadOnlyStore, error) {
	pool, err := openPool(ctx, databaseURL, passwordFile, true)
	if err != nil {
		return nil, err
	}
	return &ReadOnlyStore{pool: pool}, nil
}

func openPool(ctx context.Context, databaseURL, passwordFile string, readOnly bool) (*pgxpool.Pool, error) {
	password, err := readPassword(passwordFile)
	if err != nil {
		return nil, err
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	config.ConnConfig.Password = password
	if readOnly {
		if config.ConnConfig.RuntimeParams == nil {
			config.ConnConfig.RuntimeParams = make(map[string]string)
		}
		config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

func sameRoles(left, right []role.ID) bool {
	if len(left) != len(right) {
		return false
	}
	seen := make(map[role.ID]struct{}, len(left))
	for _, id := range left {
		seen[id] = struct{}{}
	}
	for _, id := range right {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	return true
}

func (store *Store) rolesUsingToolAuthority(tool string, authority role.CredentialAuthority) []string {
	roles := make([]string, 0, len(store.policies.Roles()))
	for _, roleID := range store.policies.Roles() {
		policy, ok := store.policies.Lookup(roleID)
		if selected, granted := policy.CredentialAuthorityForTool(tool); ok && granted && selected == authority {
			roles = append(roles, string(roleID))
		}
	}
	return roles
}

// Ready reports whether PostgreSQL can answer a request through the pool.
func (store *Store) Ready(ctx context.Context) error {
	return ready(ctx, storePool(store))
}

// CheckMigrations verifies that the database schema matches the bundled migration history.
func (store *Store) CheckMigrations(ctx context.Context) error {
	return checkMigrations(ctx, storePool(store))
}

// Close releases all PostgreSQL connections owned by the store.
func (store *Store) Close() {
	closePool(storePool(store))
}

// Ready reports whether PostgreSQL can answer a request through the read-only pool.
func (store *ReadOnlyStore) Ready(ctx context.Context) error {
	return ready(ctx, readOnlyStorePool(store))
}

// CheckMigrations verifies that the read-only database schema matches the bundled migration history.
func (store *ReadOnlyStore) CheckMigrations(ctx context.Context) error {
	return checkMigrations(ctx, readOnlyStorePool(store))
}

// Close releases all PostgreSQL connections owned by the read-only store.
func (store *ReadOnlyStore) Close() {
	closePool(readOnlyStorePool(store))
}

func ready(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("database store is not open")
	}
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

func checkMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("database store is not open")
	}
	return migrations.Check(ctx, pool)
}

func closePool(pool *pgxpool.Pool) {
	if pool != nil {
		pool.Close()
	}
}

func storePool(store *Store) *pgxpool.Pool {
	if store == nil {
		return nil
	}
	return store.pool
}

func readOnlyStorePool(store *ReadOnlyStore) *pgxpool.Pool {
	if store == nil {
		return nil
	}
	return store.pool
}

func readPassword(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read database password secret: %w", err)
	}
	password := strings.TrimRight(string(contents), "\r\n")
	if password == "" {
		return "", errors.New("database password secret is empty")
	}
	return password, nil
}

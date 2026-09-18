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

// Open connects to PostgreSQL, verifies connectivity, and applies migrations.
func Open(ctx context.Context, databaseURL, passwordFile string) (*Store, error) {
	return OpenWithReducerAndPolicies(ctx, databaseURL, passwordFile, workflow.BuiltinReducer(), role.BuiltinPolicyCatalog())
}

// OpenWithReducer connects to PostgreSQL and installs the deployment's constructed Workflow Reducer.
func OpenWithReducer(ctx context.Context, databaseURL, passwordFile string, reducer workflow.Reducer) (*Store, error) {
	policies, err := role.NewBuiltinPolicyCatalog(reducer.Roles())
	if err != nil {
		return nil, fmt.Errorf("open database store: configure Role policies: %w", err)
	}
	return OpenWithReducerAndPolicies(ctx, databaseURL, passwordFile, reducer, policies)
}

// OpenWithReducerAndPolicies connects to PostgreSQL and installs the deployment's Workflow and Role policies.
func OpenWithReducerAndPolicies(ctx context.Context, databaseURL, passwordFile string, reducer workflow.Reducer, policies role.PolicyCatalog) (*Store, error) {
	if !reducer.Valid() {
		return nil, errors.New("open database store: invalid Workflow Reducer")
	}
	if !sameRoles(reducer.Roles(), policies.Roles()) {
		return nil, errors.New("open database store: Role policies do not match Workflow Definition")
	}
	if err := reducer.ValidateRolePolicies(policies); err != nil {
		return nil, fmt.Errorf("open database store: validate Role capabilities: %w", err)
	}
	store, err := open(ctx, databaseURL, passwordFile, false)
	if err != nil {
		return nil, err
	}
	if err := migrations.Run(ctx, store.pool); err != nil {
		store.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	store.reducer = reducer
	store.policies = policies
	return store, nil
}

// OpenReadOnly connects to PostgreSQL without applying migrations and makes every connection read-only.
func OpenReadOnly(ctx context.Context, databaseURL, passwordFile string) (*Store, error) {
	return open(ctx, databaseURL, passwordFile, true)
}

func open(ctx context.Context, databaseURL, passwordFile string, readOnly bool) (*Store, error) {
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
	return &Store{pool: pool, reducer: workflow.BuiltinReducer(), policies: role.BuiltinPolicyCatalog()}, nil
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
	if store == nil || store.pool == nil {
		return errors.New("database store is not open")
	}
	if err := store.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// CheckMigrations verifies that the database schema matches the bundled migration history.
func (store *Store) CheckMigrations(ctx context.Context) error {
	if store == nil || store.pool == nil {
		return errors.New("database store is not open")
	}
	return migrations.Check(ctx, store.pool)
}

// Close releases all PostgreSQL connections owned by the store.
func (store *Store) Close() {
	if store != nil && store.pool != nil {
		store.pool.Close()
	}
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

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jozala/omnigrex/internal/store/migrations"
)

// Store owns the application's PostgreSQL connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to PostgreSQL, verifies connectivity, and applies migrations.
func Open(ctx context.Context, databaseURL, passwordFile string) (*Store, error) {
	store, err := open(ctx, databaseURL, passwordFile, false)
	if err != nil {
		return nil, err
	}
	if err := migrations.Run(ctx, store.pool); err != nil {
		store.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}
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
	return &Store{pool: pool}, nil
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

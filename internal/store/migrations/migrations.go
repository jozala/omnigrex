package migrations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var migrationFilename = regexp.MustCompile(`^([0-9]{6})_([a-z0-9][a-z0-9_]*)\.sql$`)

const (
	migrationLockID     = int64(0x4f4d4e4947524558)
	schemaMigrationsDDL = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version BIGINT PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL CHECK (name <> ''),
    checksum BYTEA NOT NULL CHECK (octet_length(checksum) = 32),
    applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
)`
)

type migration struct {
	version  int64
	name     string
	sql      string
	checksum [sha256.Size]byte
}

// Files contains the ordered SQL migrations bundled with the service.
//
//go:embed *.sql
var Files embed.FS

// Run applies every pending embedded migration in version order.
func Run(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("run migrations: nil PostgreSQL pool")
	}

	migrations, err := discover(Files)
	if err != nil {
		return err
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin migration transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("create migration history: %w", err)
	}

	for _, migration := range migrations {
		if err := apply(ctx, tx, migration); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

func discover(files fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	migrations := make([]migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || path.Ext(entry.Name()) != ".sql" {
			continue
		}

		matches := migrationFilename.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil, fmt.Errorf("invalid migration filename %q", entry.Name())
		}
		version, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse migration version %q: %w", entry.Name(), err)
		}
		if version == 0 {
			return nil, fmt.Errorf("migration version must be positive in %q", entry.Name())
		}
		contents, err := fs.ReadFile(files, entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", entry.Name(), err)
		}
		migrations = append(migrations, migration{
			version:  version,
			name:     matches[2],
			sql:      string(contents),
			checksum: sha256.Sum256(contents),
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].version < migrations[j].version
	})
	for i := 1; i < len(migrations); i++ {
		if migrations[i-1].version == migrations[i].version {
			return nil, fmt.Errorf("duplicate migration version %06d", migrations[i].version)
		}
	}
	return migrations, nil
}

func apply(ctx context.Context, tx pgx.Tx, migration migration) error {
	var recordedName string
	var recordedChecksum []byte
	err := tx.QueryRow(ctx, `
SELECT name, checksum
FROM schema_migrations
WHERE version = $1`, migration.version).Scan(&recordedName, &recordedChecksum)
	if err == nil {
		if recordedName != migration.name || !bytes.Equal(recordedChecksum, migration.checksum[:]) {
			return fmt.Errorf("migration %06d_%s.sql differs from recorded history", migration.version, migration.name)
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("read migration %06d history: %w", migration.version, err)
	}

	if _, err := tx.Exec(ctx, migration.sql); err != nil {
		return fmt.Errorf("apply migration %06d_%s.sql: %w", migration.version, migration.name, err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO schema_migrations (version, name, checksum)
VALUES ($1, $2, $3)`, migration.version, migration.name, migration.checksum[:]); err != nil {
		return fmt.Errorf("record migration %06d_%s.sql: %w", migration.version, migration.name, err)
	}
	return nil
}

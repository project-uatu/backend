// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const migrationLockID int64 = 0x554154555343484d

//go:embed migrations/*.sql
var migrationFiles embed.FS

type migration struct {
	version  int
	name     string
	sql      string
	checksum string
}

func loadMigrations() ([]migration, error) {
	names, err := fs.Glob(migrationFiles, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list database migrations: %w", err)
	}
	sort.Strings(names)
	migrations := make([]migration, 0, len(names))
	for _, name := range names {
		base := path.Base(name)
		prefix, _, ok := strings.Cut(base, "_")
		if !ok {
			return nil, fmt.Errorf("invalid migration filename %q", base)
		}
		version, err := strconv.Atoi(prefix)
		if err != nil || version < 1 {
			return nil, fmt.Errorf("invalid migration version in %q", base)
		}
		content, err := migrationFiles.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", base, err)
		}
		hash := sha256.Sum256(content)
		migrations = append(migrations, migration{
			version:  version,
			name:     base,
			sql:      string(content),
			checksum: hex.EncodeToString(hash[:]),
		})
	}
	for i, migration := range migrations {
		if migration.version != i+1 {
			return nil, fmt.Errorf("database migrations must be sequential: found version %d at position %d", migration.version, i+1)
		}
	}
	return migrations, nil
}

func migrate(ctx context.Context, pool *pgxpool.Pool) (migrationErr error) {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin database migration: %w", err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			migrationErr = errors.Join(migrationErr, fmt.Errorf("rollback database migration: %w", err))
		}
	}()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
		return fmt.Errorf("lock database migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			checksum TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	rows, err := tx.Query(ctx, "SELECT version, name, checksum FROM schema_migrations ORDER BY version")
	if err != nil {
		return fmt.Errorf("read applied migrations: %w", err)
	}
	applied := make(map[int]migration, len(migrations))
	for rows.Next() {
		var item migration
		if err := rows.Scan(&item.version, &item.name, &item.checksum); err != nil {
			rows.Close()
			return fmt.Errorf("scan applied migration: %w", err)
		}
		applied[item.version] = item
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate applied migrations: %w", err)
	}
	rows.Close()

	if len(applied) > 0 && len(migrations) > 0 {
		for version := range applied {
			if version > migrations[len(migrations)-1].version {
				return fmt.Errorf("database schema version %d is newer than supported version %d", version, migrations[len(migrations)-1].version)
			}
		}
	}
	for _, item := range migrations {
		if existing, ok := applied[item.version]; ok {
			if existing.name != item.name || existing.checksum != item.checksum {
				return fmt.Errorf("database migration %d checksum does not match %s", item.version, item.name)
			}
			continue
		}
		for version := range applied {
			if version > item.version {
				return fmt.Errorf("database migration history is missing version %d", item.version)
			}
		}
		if _, err := tx.Exec(ctx, item.sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			return fmt.Errorf("apply database migration %s: %w", item.name, err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
			item.version, item.name, item.checksum); err != nil {
			return fmt.Errorf("record database migration %s: %w", item.name, err)
		}
		applied[item.version] = item
	}
	if len(applied) != len(migrations) {
		return fmt.Errorf("applied migration count %d does not match supported count %d", len(applied), len(migrations))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit database migrations: %w", err)
	}
	committed = true
	return nil
}

// checkSchema verifies that every migration known to this binary is applied
// with an unchanged checksum. Extra, newer migrations are allowed.
func checkSchema(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := loadMigrations()
	if err != nil {
		return err
	}
	rows, err := pool.Query(ctx, "SELECT version, name, checksum FROM schema_migrations")
	if err != nil {
		return schemaQueryError(err)
	}
	defer rows.Close()
	applied := make(map[int]migration, len(migrations))
	for rows.Next() {
		var item migration
		if err := rows.Scan(&item.version, &item.name, &item.checksum); err != nil {
			return fmt.Errorf("scan applied migration: %w", err)
		}
		applied[item.version] = item
	}
	if err := rows.Err(); err != nil {
		return schemaQueryError(err)
	}
	for _, item := range migrations {
		existing, ok := applied[item.version]
		if !ok {
			return fmt.Errorf("%w: migration %s is not applied; run uatu migrate", ErrSchemaIncompatible, item.name)
		}
		if existing.name != item.name || existing.checksum != item.checksum {
			return fmt.Errorf("%w: migration %d does not match %s", ErrSchemaIncompatible, item.version, item.name)
		}
	}
	return nil
}

func schemaQueryError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
		return fmt.Errorf("%w: schema_migrations is missing; run uatu migrate", ErrSchemaIncompatible)
	}
	return fmt.Errorf("read applied migrations: %w", err)
}

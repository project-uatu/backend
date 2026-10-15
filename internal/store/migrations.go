package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const currentSchemaVersion = 1

const createSnapshotsTable = `
CREATE TABLE snapshots (
	source     TEXT    NOT NULL,
	username   TEXT    NOT NULL,
	data       BLOB    NOT NULL,
	fetched_at INTEGER NOT NULL,
	PRIMARY KEY (source, username)
);`

func migrate(ctx context.Context, db *sql.DB) (migrationErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration: %w", err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			migrationErr = errors.Join(migrationErr, fmt.Errorf("rollback migration: %w", err))
		}
	}()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, currentSchemaVersion)
	}
	if version == currentSchemaVersion {
		return tx.Commit()
	}

	var hasSnapshots bool
	if err := tx.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'snapshots')",
	).Scan(&hasSnapshots); err != nil {
		return fmt.Errorf("check existing schema: %w", err)
	}
	if hasSnapshots {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE snapshots RENAME TO snapshots_legacy"); err != nil {
			return fmt.Errorf("rename legacy snapshots table: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, createSnapshotsTable); err != nil {
		return fmt.Errorf("create snapshots table: %w", err)
	}
	if hasSnapshots {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO snapshots (source, username, data, fetched_at)
			SELECT source, username, data, fetched_at FROM snapshots_legacy`); err != nil {
			return fmt.Errorf("copy legacy snapshots: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DROP TABLE snapshots_legacy"); err != nil {
			return fmt.Errorf("drop legacy snapshots table: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion)); err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return tx.Commit()
}

// Package store persists collected results in a SQLite database so they
// survive between runs of Uatu.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Source identifies where a stored result was collected from.
type Source string

// Known result sources.
const (
	SourceGitHubRepos Source = "github_repos"
	SourceDevStats    Source = "devstats"
)

// Snapshot is a collected result for a user from a source.
type Snapshot struct {
	Source    Source
	User      string
	Data      []byte
	FetchedAt time.Time
}

// Store is a SQLite-backed result store.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	// SQLite allows a single writer; one connection avoids lock contention.
	db.SetMaxOpenConns(1)

	if err := migrate(ctx, db); err != nil {
		return nil, errors.Join(fmt.Errorf("migrate database: %w", err), db.Close())
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Save stores data as the latest snapshot for user and source.
func (s *Store) Save(ctx context.Context, source Source, user string, data []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO snapshots (source, username, data, fetched_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (source, username)
		DO UPDATE SET data = excluded.data, fetched_at = excluded.fetched_at`,
		string(source), user, data, time.Now().UTC().Unix())
	if err != nil {
		return fmt.Errorf("save snapshot %s/%s: %w", source, user, err)
	}
	return nil
}

// Get returns the latest snapshot for user and source. The boolean is false
// when nothing has been stored yet.
func (s *Store) Get(ctx context.Context, source Source, user string) (Snapshot, bool, error) {
	var (
		data      []byte
		fetchedAt int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT data, fetched_at FROM snapshots WHERE source = ? AND username = ?`,
		string(source), user).Scan(&data, &fetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("get snapshot %s/%s: %w", source, user, err)
	}
	return Snapshot{
		Source:    source,
		User:      user,
		Data:      data,
		FetchedAt: time.Unix(fetchedAt, 0).UTC(),
	}, true, nil
}

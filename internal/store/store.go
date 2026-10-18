// SPDX-License-Identifier: Apache-2.0

// Package store persists Uatu data in PostgreSQL.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SourceGitHubHistory identifies person-centric GitHub activity collection.
const SourceGitHubHistory = "github_history"

// Options configures the PostgreSQL connection pool.
type Options struct {
	URL            string
	SSLMode        string
	MinConns       int32
	MaxConns       int32
	ConnectTimeout time.Duration
}

// Store persists source data and person-centric contribution history.
type Store struct {
	db *pgxpool.Pool
}

// ErrSchemaIncompatible reports that the database schema lacks migrations the
// running binary requires.
var ErrSchemaIncompatible = errors.New("database schema is not compatible with this release")

// Open connects to PostgreSQL and verifies that the schema contains every
// migration this release needs. It never changes the schema; run Migrate first.
// A schema that is newer than this release is accepted so that old replicas
// keep serving during a rolling upgrade.
func Open(ctx context.Context, options Options) (*Store, error) {
	pool, err := openPool(ctx, options)
	if err != nil {
		return nil, err
	}
	if err := checkSchema(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{db: pool}, nil
}

// Migrate applies pending schema migrations under a database-wide lock, so
// concurrent callers are serialized, and then closes its connections.
func Migrate(ctx context.Context, options Options) error {
	pool, err := openPool(ctx, options)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate PostgreSQL database: %w", err)
	}
	return nil
}

// Ready reports whether the database is reachable and has a compatible schema.
func (s *Store) Ready(ctx context.Context) error {
	return checkSchema(ctx, s.db)
}

func openPool(ctx context.Context, options Options) (*pgxpool.Pool, error) {
	if options.URL == "" {
		return nil, errors.New("UATU_DATABASE_URL is required")
	}
	if options.MinConns < 0 || options.MaxConns <= 0 || options.MaxConns < options.MinConns {
		return nil, errors.New("database connection limits must satisfy 0 <= min <= max and max > 0")
	}
	if options.ConnectTimeout <= 0 {
		return nil, errors.New("database connect timeout must be positive")
	}
	databaseURL, err := withDefaultSSLMode(options.URL, options.SSLMode)
	if err != nil {
		return nil, err
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL database URL")
	}
	config.MinConns = options.MinConns
	config.MaxConns = options.MaxConns
	config.ConnConfig.ConnectTimeout = options.ConnectTimeout

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("configure PostgreSQL connection pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	return pool, nil
}

func withDefaultSSLMode(databaseURL, sslMode string) (string, error) {
	parsed, err := url.Parse(databaseURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		return "", errors.New("UATU_DATABASE_URL must be a PostgreSQL URL")
	}
	if parsed.Query().Has("sslmode") {
		return databaseURL, nil
	}
	if sslMode == "" {
		sslMode = "verify-full"
	}
	query := parsed.Query()
	query.Set("sslmode", sslMode)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// Close releases the underlying database connection pool.
func (s *Store) Close() {
	s.db.Close()
}

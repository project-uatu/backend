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

// Open connects to PostgreSQL, verifies the connection and applies migrations.
func Open(ctx context.Context, options Options) (*Store, error) {
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
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate PostgreSQL database: %w", err)
	}
	return &Store{db: pool}, nil
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

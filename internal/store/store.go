// Package store persists Uatu data in PostgreSQL.
package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Source identifies where a stored result was collected from.
type Source string

// SourceGitHubRepositories identifies GitHub repository listings.
const SourceGitHubRepositories Source = "github_repositories"

// Snapshot is a collected result for a user from a source.
type Snapshot struct {
	Source    Source
	User      string
	Data      []byte
	FetchedAt time.Time
}

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

// Save stores data as the latest snapshot for user and source.
func (s *Store) Save(ctx context.Context, source Source, user string, data []byte) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO snapshots (source, username, data, fetched_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (source, username)
		DO UPDATE SET data = EXCLUDED.data, fetched_at = EXCLUDED.fetched_at`,
		string(source), user, data, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("save snapshot %s/%s: %w", source, user, err)
	}
	return nil
}

// Get returns the latest snapshot for user and source. The boolean is false
// when nothing has been stored yet.
func (s *Store) Get(ctx context.Context, source Source, user string) (Snapshot, bool, error) {
	var snapshot Snapshot
	snapshot.Source = source
	snapshot.User = user
	err := s.db.QueryRow(ctx,
		`SELECT data, fetched_at FROM snapshots WHERE source = $1 AND username = $2`,
		string(source), user).Scan(&snapshot.Data, &snapshot.FetchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("get snapshot %s/%s: %w", source, user, err)
	}
	snapshot.FetchedAt = snapshot.FetchedAt.UTC()
	return snapshot, true, nil
}

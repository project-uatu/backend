package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func TestStore_persistsBetweenOpens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "uatu.db")

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok, err := s.Get(ctx, SourceGitHubRepos, "octocat"); err != nil || ok {
		t.Fatalf("Get on empty store = ok %v, err %v; want false, nil", ok, err)
	}
	if err := s.Save(ctx, SourceGitHubRepos, "octocat", []byte("first")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Save(ctx, SourceGitHubRepos, "octocat", []byte("second")); err != nil {
		t.Fatalf("Save overwrite: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close reopened store: %v", err)
		}
	})

	snap, ok, err := s.Get(ctx, SourceGitHubRepos, "octocat")
	if err != nil || !ok {
		t.Fatalf("Get after reopen = ok %v, err %v; want true, nil", ok, err)
	}
	if string(snap.Data) != "second" {
		t.Errorf("Data = %q, want %q", snap.Data, "second")
	}
	if snap.FetchedAt.IsZero() {
		t.Error("FetchedAt is zero")
	}
	if _, ok, err := s.Get(ctx, Source("other"), "octocat"); err != nil {
		t.Fatalf("Get other source: %v", err)
	} else if ok {
		t.Error("snapshot leaked across sources")
	}
}

func TestOpen_migratesLegacySchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacyDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = legacyDB.ExecContext(ctx, `
		CREATE TABLE snapshots (
			source TEXT NOT NULL,
			username TEXT NOT NULL,
			data BLOB NOT NULL,
			fetched_at INTEGER NOT NULL,
			PRIMARY KEY (source, username)
		);
		INSERT INTO snapshots VALUES ('github_repos', 'octocat', 'repo-one', 1760540400);`)
	if err != nil {
		t.Fatalf("create legacy snapshot: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open legacy database: %v", err)
	}
	snapshot, ok, err := s.Get(ctx, SourceGitHubRepos, "octocat")
	if err != nil || !ok {
		t.Fatalf("Get migrated snapshot = ok %v, err %v; want true, nil", ok, err)
	}
	if string(snapshot.Data) != "repo-one" {
		t.Errorf("snapshot data = %q, want legacy payload", snapshot.Data)
	}
	if got := snapshot.FetchedAt.Unix(); got != 1760540400 {
		t.Errorf("snapshot timestamp = %d, want 1760540400", got)
	}

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != currentSchemaVersion {
		t.Errorf("schema version = %d, want %d", version, currentSchemaVersion)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen migrated database: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close reopened store: %v", err)
	}
}

func TestOpen_rejectsNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion+1)); err != nil {
		t.Fatalf("set schema version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close test database: %v", err)
	}

	_, err = Open(context.Background(), path)
	if err == nil {
		t.Fatal("Open newer schema = nil, want error")
	}
}

package config

import (
	"reflect"
	"testing"
	"time"
)

func TestParseUsers_usesArgumentsOrFallback(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		fallback []string
		want     []string
		wantErr  bool
	}{
		{name: "arguments", args: []string{"alice", " bob "}, fallback: []string{"ignored"}, want: []string{"alice", "bob"}},
		{name: "fallback", fallback: []string{"alice,bob"}, want: []string{"alice", "bob"}},
		{name: "empty", args: []string{" ", ""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseUsers(tt.args, tt.fallback)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseUsers error = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseUsers = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestLoad_readsEnvironment(t *testing.T) {
	t.Setenv("UATU_DATABASE_URL", "postgres://uatu:secret@localhost/uatu")
	t.Setenv("UATU_DB_SSLMODE", "verify-full")
	t.Setenv("UATU_DB_MIN_CONNS", "1")
	t.Setenv("UATU_DB_MAX_CONNS", "12")
	t.Setenv("UATU_DB_CONNECT_TIMEOUT", "3s")
	t.Setenv("UATU_ADDR", "127.0.0.1:4000")
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("UATU_USERS", "alice, bob")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DatabaseURL != "postgres://uatu:secret@localhost/uatu" ||
		got.DatabaseSSLMode != "verify-full" ||
		got.DatabaseMinConns != 1 || got.DatabaseMaxConns != 12 ||
		got.DatabaseConnectTimeout != 3*time.Second ||
		got.Address != "127.0.0.1:4000" || got.GitHubToken != "test-token" ||
		!reflect.DeepEqual(got.Users, []string{"alice", "bob"}) {
		t.Errorf("Load = %+v, want configured values", got)
	}
}

func TestLoad_defaultsDatabasePool(t *testing.T) {
	t.Setenv("UATU_DATABASE_URL", "postgres://uatu@localhost/uatu")
	t.Setenv("UATU_DB_SSLMODE", "")
	t.Setenv("UATU_DB_MIN_CONNS", "")
	t.Setenv("UATU_DB_MAX_CONNS", "")
	t.Setenv("UATU_DB_CONNECT_TIMEOUT", "")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DatabaseSSLMode != "verify-full" ||
		got.DatabaseMinConns != 0 ||
		got.DatabaseMaxConns != 10 ||
		got.DatabaseConnectTimeout != 5*time.Second {
		t.Errorf("Load database defaults = %+v, want secure TLS and default pool values", got)
	}
}

func TestLoad_rejectsInvalidPoolSettings(t *testing.T) {
	tests := []struct {
		name string
		set  func(*testing.T)
	}{
		{
			name: "malformed maximum",
			set: func(t *testing.T) {
				t.Setenv("UATU_DB_MAX_CONNS", "many")
			},
		},
		{
			name: "minimum above maximum",
			set: func(t *testing.T) {
				t.Setenv("UATU_DB_MIN_CONNS", "12")
				t.Setenv("UATU_DB_MAX_CONNS", "10")
			},
		},
		{
			name: "nonpositive timeout",
			set: func(t *testing.T) {
				t.Setenv("UATU_DB_CONNECT_TIMEOUT", "0s")
			},
		},
		{
			name: "invalid ssl mode",
			set: func(t *testing.T) {
				t.Setenv("UATU_DB_SSLMODE", "trust-me")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.set(t)
			if _, err := Load(); err == nil {
				t.Fatal("Load() error = nil, want error")
			}
		})
	}
}

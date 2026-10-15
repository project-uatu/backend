package config

import (
	"reflect"
	"testing"
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
	t.Setenv("UATU_DB_PATH", "/tmp/uatu.db")
	t.Setenv("UATU_ADDR", "127.0.0.1:4000")
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("UATU_USERS", "alice, bob")

	got := Load()
	if got.DatabasePath != "/tmp/uatu.db" || got.Address != "127.0.0.1:4000" ||
		got.GitHubToken != "test-token" || !reflect.DeepEqual(got.Users, []string{"alice", "bob"}) {
		t.Errorf("Load = %+v, want configured values", got)
	}
}

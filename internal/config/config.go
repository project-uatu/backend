// Package config loads Uatu's process configuration.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config contains settings loaded once when the process starts.
type Config struct {
	DatabasePath string
	Address      string
	GitHubToken  string
	Users        []string
}

// Load reads Uatu configuration from the environment.
func Load() Config {
	return Config{
		DatabasePath: envOr("UATU_DB_PATH", "data/uatu.db"),
		Address:      envOr("UATU_ADDR", ":3000"),
		GitHubToken:  os.Getenv("GITHUB_TOKEN"),
		Users:        splitUsers(os.Getenv("UATU_USERS")),
	}
}

// ParseUsers returns trimmed, non-empty user names from args or the fallback
// environment value when args is empty.
func ParseUsers(args []string, fallback []string) ([]string, error) {
	rawUsers := args
	if len(rawUsers) == 0 {
		rawUsers = fallback
	}
	users := splitUsers(strings.Join(rawUsers, ","))
	if len(users) == 0 {
		return nil, fmt.Errorf("no users to collect; pass usernames or set UATU_USERS")
	}
	return users, nil
}

func splitUsers(value string) []string {
	var users []string
	for _, user := range strings.Split(value, ",") {
		if user = strings.TrimSpace(user); user != "" {
			users = append(users, user)
		}
	}
	return users
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

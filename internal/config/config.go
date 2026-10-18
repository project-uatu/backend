// Package config loads Uatu's process configuration.
package config

import (
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config contains settings loaded once when the process starts.
type Config struct {
	DatabaseURL            string
	DatabaseSSLMode        string
	DatabaseMinConns       int32
	DatabaseMaxConns       int32
	DatabaseConnectTimeout time.Duration
	Address                string
	HealthAddress          string
	APIKey                 string
	CORSOrigins            []string
	GitHubToken            string
	// GitLabToken and CodebergToken are optional read-only tokens.
	GitLabToken   string
	CodebergToken string
	// Forges lists the enabled forge hosts.
	Forges []string
	Users  []string
}

// Load reads Uatu configuration from the environment.
func Load() (Config, error) {
	minConns, err := envInt32("UATU_DB_MIN_CONNS", 0)
	if err != nil {
		return Config{}, err
	}
	maxConns, err := envInt32("UATU_DB_MAX_CONNS", 10)
	if err != nil {
		return Config{}, err
	}
	connectTimeout, err := envDuration("UATU_DB_CONNECT_TIMEOUT", 5*time.Second)
	if err != nil {
		return Config{}, err
	}
	if minConns < 0 || maxConns <= 0 || maxConns < minConns {
		return Config{}, fmt.Errorf("database connection limits must satisfy 0 <= min <= max and max > 0")
	}
	if connectTimeout <= 0 {
		return Config{}, fmt.Errorf("UATU_DB_CONNECT_TIMEOUT must be positive")
	}
	apiKey := os.Getenv("UATU_API_KEY")
	if apiKey != "" && (len(apiKey) < 32 || strings.ContainsAny(apiKey, " \t\r\n")) {
		return Config{}, fmt.Errorf("UATU_API_KEY must be at least 32 bytes and contain no whitespace")
	}
	corsOrigins, err := parseCORSOrigins(os.Getenv("UATU_CORS_ORIGINS"))
	if err != nil {
		return Config{}, err
	}
	sslMode := envOr("UATU_DB_SSLMODE", "verify-full")
	switch sslMode {
	case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
	default:
		return Config{}, fmt.Errorf("invalid UATU_DB_SSLMODE %q", sslMode)
	}
	forges, err := parseForges(os.Getenv("UATU_FORGES"))
	if err != nil {
		return Config{}, err
	}
	return Config{
		DatabaseURL:            os.Getenv("UATU_DATABASE_URL"),
		DatabaseSSLMode:        sslMode,
		DatabaseMinConns:       minConns,
		DatabaseMaxConns:       maxConns,
		DatabaseConnectTimeout: connectTimeout,
		Address:                envOr("UATU_ADDR", ":3000"),
		HealthAddress:          envOr("UATU_HEALTH_ADDR", ":3001"),
		APIKey:                 apiKey,
		CORSOrigins:            corsOrigins,
		GitHubToken:            os.Getenv("GITHUB_TOKEN"),
		GitLabToken:            os.Getenv("GITLAB_TOKEN"),
		CodebergToken:          os.Getenv("CODEBERG_TOKEN"),
		Forges:                 forges,
		Users:                  splitUsers(os.Getenv("UATU_USERS")),
	}, nil
}

// SupportedForges are the forge hosts Uatu can collect from.
var SupportedForges = []string{"github.com", "gitlab.com", "codeberg.org"}

func parseForges(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return append([]string(nil), SupportedForges...), nil
	}
	var forges []string
	seen := map[string]bool{}
	for _, host := range strings.Split(value, ",") {
		host = strings.ToLower(strings.TrimSpace(host))
		if !slices.Contains(SupportedForges, host) {
			return nil, fmt.Errorf("UATU_FORGES contains unsupported forge %q", host)
		}
		if seen[host] {
			return nil, fmt.Errorf("UATU_FORGES contains duplicate forge %q", host)
		}
		seen[host] = true
		forges = append(forges, host)
	}
	return forges, nil
}

// ForgeEnabled reports whether host is enabled; an unset list enables all
// supported forges.
func (c Config) ForgeEnabled(host string) bool {
	return len(c.Forges) == 0 || slices.Contains(c.Forges, host)
}

func parseCORSOrigins(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	origins := strings.Split(value, ",")
	seen := make(map[string]struct{}, len(origins))
	for i, origin := range origins {
		origin = strings.TrimSpace(origin)
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Scheme != "http" && parsed.Scheme != "https" ||
			parsed.Host == "" || parsed.User != nil || parsed.Path != "" ||
			parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || origin == "*" {
			return nil, fmt.Errorf("UATU_CORS_ORIGINS must contain exact HTTP origins")
		}
		if _, ok := seen[origin]; ok {
			return nil, fmt.Errorf("UATU_CORS_ORIGINS contains duplicate origin %q", origin)
		}
		seen[origin] = struct{}{}
		origins[i] = origin
	}
	return origins, nil
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

func envInt32(name string, fallback int32) (int32, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return int32(parsed), nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return parsed, nil
}

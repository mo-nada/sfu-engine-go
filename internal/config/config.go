// Package config loads and validates the SFU server configuration.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mo-nada/sfu-engine/internal/auth"
	"github.com/mo-nada/sfu-engine/internal/cluster"
	"github.com/mo-nada/sfu-engine/internal/store"
)

// Config holds all settings needed to run the SFU server.
type Config struct {
	// Addr is the HTTP listen address, for example ":8080".
	Addr string
	// LogLevel is one of "debug", "info", "warn", "error".
	LogLevel string
	// LogFormat is either "text" or "json".
	LogFormat string
	// STUNURLs are the STUN servers used for ICE. Empty means host candidates only.
	STUNURLs []string
	// ShutdownTimeout bounds how long a graceful shutdown may take.
	ShutdownTimeout time.Duration
	// TenantsFile is a JSON file of tenants (see auth.LoadTenants).
	TenantsFile string
	// Tenants are added programmatically (for embedding and tests).
	Tenants []auth.Tenant
	// Dev adds a development tenant with a random secret and enables
	// GET /dev/token, which gives a join token to anyone. Never use in production.
	Dev bool

	// DatabaseURL is the PostgreSQL URL for tenants, keys and usage. Empty
	// keeps them in memory (lost on restart, one node only).
	DatabaseURL string
	// RedisURL is the Redis or Valkey URL shared by all nodes. Empty means a
	// single node.
	RedisURL string
	// AdminSecret enables the operator admin API (/admin/v1); at least 32 bytes.
	AdminSecret string
	// NodeID names this node in the cluster (default: host name plus random).
	NodeID string
	// NodeURL is the base URL other nodes use to reach this node, for
	// example http://10.0.0.5:8080 (default: http://127.0.0.1:<port>).
	NodeURL string
	// DrainTimeout is how long shutdown waits for rooms to empty while the
	// node takes no new rooms. 0 shuts down at once.
	DrainTimeout time.Duration

	// For embedding and tests: ready-made stores instead of URLs, and
	// heartbeat timing (zero means the defaults).
	Store             store.Store
	ClusterStore      cluster.Store
	HeartbeatInterval time.Duration
	HeartbeatTTL      time.Duration
}

// Default returns a Config with sensible defaults.
func Default() Config {
	return Config{
		Addr:            ":8080",
		LogLevel:        "info",
		LogFormat:       "text",
		STUNURLs:        []string{"stun:stun.l.google.com:19302"},
		ShutdownTimeout: 10 * time.Second,
	}
}

// Load parses command-line arguments (without the program name) on top of
// the defaults and validates the result.
func Load(args []string) (Config, error) {
	cfg := Default()

	fs := flag.NewFlagSet("sfu", flag.ContinueOnError)
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "HTTP listen address")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log level: debug, info, warn, error")
	fs.StringVar(&cfg.LogFormat, "log-format", cfg.LogFormat, "log format: text or json")
	stun := fs.String("stun", strings.Join(cfg.STUNURLs, ","), "comma-separated STUN server URLs (empty to disable)")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", cfg.ShutdownTimeout, "maximum time for graceful shutdown")
	fs.StringVar(&cfg.TenantsFile, "tenants", "", "JSON file with tenants and their API keys and secrets")
	fs.BoolVar(&cfg.Dev, "dev", false, "development mode: dev tenant and open /dev/token endpoint (never in production)")
	fs.StringVar(&cfg.DatabaseURL, "database-url", os.Getenv("SFU_DATABASE_URL"), "PostgreSQL URL for tenants, keys and usage (env SFU_DATABASE_URL); empty keeps them in memory")
	fs.StringVar(&cfg.RedisURL, "redis-url", os.Getenv("SFU_REDIS_URL"), "Redis/Valkey URL shared by all nodes (env SFU_REDIS_URL); empty means one node")
	fs.StringVar(&cfg.AdminSecret, "admin-secret", os.Getenv("SFU_ADMIN_SECRET"), "enables the admin API (prefer env SFU_ADMIN_SECRET; at least 32 bytes)")
	fs.StringVar(&cfg.NodeID, "node-id", os.Getenv("SFU_NODE_ID"), "node name in the cluster (env SFU_NODE_ID)")
	fs.StringVar(&cfg.NodeURL, "node-url", os.Getenv("SFU_NODE_URL"), "URL other nodes use to reach this node (env SFU_NODE_URL)")
	fs.DurationVar(&cfg.DrainTimeout, "drain-timeout", 0, "on shutdown, wait this long for rooms to empty (0: stop at once)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	cfg.STUNURLs = splitList(*stun)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate reports every invalid setting in the config.
func (c Config) Validate() error {
	var errs []error
	if c.Addr == "" {
		errs = append(errs, errors.New("addr must not be empty"))
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		errs = append(errs, fmt.Errorf("invalid log level %q", c.LogLevel))
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		errs = append(errs, fmt.Errorf("invalid log format %q", c.LogFormat))
	}
	for _, u := range c.STUNURLs {
		if !strings.HasPrefix(u, "stun:") && !strings.HasPrefix(u, "stuns:") {
			errs = append(errs, fmt.Errorf("invalid STUN URL %q: must start with stun: or stuns:", u))
		}
	}
	if c.TenantsFile == "" && len(c.Tenants) == 0 && !c.Dev && c.DatabaseURL == "" && c.Store == nil && c.AdminSecret == "" {
		errs = append(errs, errors.New("no tenants: pass -tenants FILE, -database-url, -admin-secret, or -dev for local development"))
	}
	if c.AdminSecret != "" && len(c.AdminSecret) < 32 {
		errs = append(errs, errors.New("admin secret must be at least 32 bytes"))
	}
	if c.NodeURL != "" && !strings.HasPrefix(c.NodeURL, "http://") && !strings.HasPrefix(c.NodeURL, "https://") {
		errs = append(errs, errors.New("node url must start with http:// or https://"))
	}
	if c.DrainTimeout < 0 {
		errs = append(errs, errors.New("drain timeout must not be negative"))
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, errors.New("shutdown timeout must be positive"))
	}
	return errors.Join(errs...)
}

// splitList splits a comma-separated list, trimming spaces and dropping empty items.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

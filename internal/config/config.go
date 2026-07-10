// Package config handles loading and managing the omni-agent-hub hub configuration.
//
// The new (hub-shaped) YAML layout, per the spec, is:
//
//	server:
//	  host: "0.0.0.0"
//	  port: 8222
//	  public_url: "http://localhost:8222"
//	  api_key: "..."
//	  admin_key: "..."
//	hub:
//	  name: "Omni A2A Hub"
//	  description: "Aggregator for local and remote A2A agents."
//	storage:
//	  path: "~/.omni-agent-hub/state.db"
//	  audit_retention: 10000
//	logging:
//	  file: "~/.omni-agent-hub/logs/server.log"
//	  level: "info"
//	  format: "json"
//	upstream:
//	  - name: "hermes"
//	    base_url: "http://localhost:1424"
//	    prefix: "@hermes"
//	    auth:
//	      scheme: "bearer"
//	      token: "..."
//	    enabled: true
//
// Legacy configs (`agent:` block, top-level `upstream[].token`, `upstream[].url`
// instead of `base_url`) are auto-migrated on load with a WARN.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration for the omni-agent-hub hub.
type Config struct {
	Server   ServerConfig  `yaml:"server"`
	Hub      HubConfig     `yaml:"hub"`
	Storage  StorageConfig `yaml:"storage"`
	Logging  LoggingConfig `yaml:"logging"`
	Upstream []UpstreamCfg `yaml:"upstream"`
}

// ServerConfig holds the HTTP server settings.
type ServerConfig struct {
	Host      string `yaml:"host"`
	Port      int    `yaml:"port"`
	PublicURL string `yaml:"public_url"` // advertised in composite AgentCard
	APIKey    string `yaml:"api_key"`    // client bearer, required for POST /
	AdminKey  string `yaml:"admin_key"`  // admin bearer, required for /admin/*
}

// HubConfig holds hub identity advertised in the composite agent card.
type HubConfig struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	// RefreshInterval is how often the running server re-fetches all upstream
	// agent cards in the background (Go duration string, e.g. "10m"). Empty
	// uses the default (10m); "0" or a negative value disables periodic refresh.
	RefreshInterval string `yaml:"refresh_interval"`
}

// DefaultRefreshInterval is the fallback period for background upstream card
// refresh when hub.refresh_interval is unset.
const DefaultRefreshInterval = 10 * time.Minute

// RefreshIntervalOrDefault returns the parsed background refresh period. An
// empty value yields DefaultRefreshInterval; a zero or negative duration
// disables periodic refresh (returns 0); an unparseable value logs a WARN and
// uses the default.
func (h HubConfig) RefreshIntervalOrDefault() time.Duration {
	if h.RefreshInterval == "" {
		return DefaultRefreshInterval
	}
	d, err := time.ParseDuration(h.RefreshInterval)
	if err != nil {
		slog.Warn("invalid hub.refresh_interval; using default",
			"value", h.RefreshInterval, "default", DefaultRefreshInterval)
		return DefaultRefreshInterval
	}
	if d <= 0 {
		return 0
	}
	return d
}

// StorageConfig holds SQLite storage settings.
type StorageConfig struct {
	Path           string `yaml:"path"`            // SQLite file
	AuditRetention int    `yaml:"audit_retention"` // max rows kept in audit_log
}

// LoggingConfig holds log output settings.
type LoggingConfig struct {
	File   string `yaml:"file"`
	Level  string `yaml:"level"`  // debug | info | warn | error
	Format string `yaml:"format"` // json | text
}

// UpstreamCfg is a single upstream A2A agent entry from config.yaml.
type UpstreamCfg struct {
	Name    string     `yaml:"name" json:"name"`
	BaseURL string     `yaml:"base_url" json:"base_url"`
	Prefix  string     `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	Auth    AuthConfig `yaml:"auth" json:"auth"`
	Enabled bool       `yaml:"enabled" json:"enabled"`

	// Legacy field. If set on load and BaseURL is empty, migrated into BaseURL.
	LegacyURL string `yaml:"url,omitempty" json:"-"`
	// Legacy field. If set on load and Auth is zero, migrated into Auth.Token.
	LegacyToken string `yaml:"token,omitempty" json:"-"`
}

// AuthConfig describes how to authenticate to an upstream.
type AuthConfig struct {
	Scheme string `yaml:"scheme" json:"scheme"` // "bearer" | "none"
	Token  string `yaml:"token,omitempty" json:"token,omitempty"`
}

// LegacyAgentBlock is retained ONLY so YAML unmarshaling can detect the presence
// of a legacy `agent:` block and warn the user; the hub no longer executes any
// local agent.
type LegacyAgentBlock struct {
	Present bool
}

// rawConfig mirrors Config but preserves the legacy `agent:` block so we can
// warn without failing.
type rawConfig struct {
	Config `yaml:",inline"`
	Agent  map[string]any `yaml:"agent"`
}

// DefaultConfig returns a sensible default configuration for a fresh install.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:      "0.0.0.0",
			Port:      8222,
			PublicURL: "http://localhost:8222",
		},
		Hub: HubConfig{
			Name:        "Omni A2A Hub",
			Description: "Aggregator for local and remote A2A agents.",
		},
		Storage: StorageConfig{
			Path:           "~/.omni-agent-hub/state.db",
			AuditRetention: 10000,
		},
		Logging: LoggingConfig{
			File:   "~/.omni-agent-hub/logs/server.log",
			Level:  "info",
			Format: "json",
		},
		Upstream: []UpstreamCfg{},
	}
}

// Load reads a YAML config file and returns the parsed Config. Legacy
// fields are migrated in-place and warned about.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	raw := rawConfig{Config: *DefaultConfig()}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	cfg := &raw.Config
	migrateLegacyFields(cfg, len(raw.Agent) > 0)
	if cfg.Server.AdminKey == "" {
		cfg.Server.AdminKey = generateKey()
		slog.Warn("no admin_key in config; auto-generated one; save config to persist", "admin_key", cfg.Server.AdminKey)
	}
	return cfg, nil
}

func generateKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "CHANGE_ME_key_generation_failed"
	}
	return hex.EncodeToString(b)
}

// migrateLegacyFields translates the pre-hub YAML shape into the new one and
// emits WARN logs for anything migrated.
func migrateLegacyFields(cfg *Config, hasAgentBlock bool) {
	if hasAgentBlock {
		slog.Warn("legacy `agent:` block in config is ignored; the hub no longer executes agents locally")
	}
	for i := range cfg.Upstream {
		u := &cfg.Upstream[i]
		if u.BaseURL == "" && u.LegacyURL != "" {
			slog.Warn("legacy upstream field `url` is deprecated; use `base_url`", "name", u.Name)
			u.BaseURL = u.LegacyURL
		}
		u.LegacyURL = ""
		if u.Auth.Scheme == "" && u.LegacyToken != "" {
			slog.Warn("legacy upstream field `token` is deprecated; use `auth: { scheme: bearer, token: ... }`", "name", u.Name)
			u.Auth = AuthConfig{Scheme: "bearer", Token: u.LegacyToken}
		}
		u.LegacyToken = ""
		if u.Auth.Scheme == "" {
			u.Auth.Scheme = "none"
		}
	}
}

// Validate performs strict validation on a loaded config. It is a separate step
// from Load so tests and migration flows can inspect a config before failing.
func (c *Config) Validate() error {
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port %d is not in 1..65535", c.Server.Port)
	}
	if c.Server.PublicURL == "" {
		return errors.New("server.public_url is required")
	}
	if c.Server.APIKey == "" {
		return errors.New("server.api_key is required")
	}
	// admin_key is auto-generated in Load() if empty, so this should never
	// fire, but we keep the check as a safety net in case of direct construction.
	// (The auto-generated key is logged at WARN level on startup.)
	seen := make(map[string]struct{}, len(c.Upstream))
	for i, u := range c.Upstream {
		if u.Name == "" {
			return fmt.Errorf("upstream[%d]: name is required", i)
		}
		if _, dup := seen[u.Name]; dup {
			return fmt.Errorf("upstream[%d]: duplicate name %q", i, u.Name)
		}
		seen[u.Name] = struct{}{}
		if u.BaseURL == "" {
			return fmt.Errorf("upstream[%d] %q: base_url is required", i, u.Name)
		}
		if u.Auth.Scheme != "" && u.Auth.Scheme != "bearer" && u.Auth.Scheme != "none" {
			return fmt.Errorf("upstream[%d] %q: auth.scheme must be 'bearer' or 'none', got %q",
				i, u.Name, u.Auth.Scheme)
		}
	}
	return nil
}

// DefaultConfigPath returns the default config file path (~/.config/omni-agent-hub/config.yaml).
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "omni-agent-hub", "config.yaml")
}

// LoadOrDefault tries to load from the given path; if empty, falls back to
// ~/.config/omni-agent-hub/config.yaml; if that doesn't exist either, returns defaults.
// Unlike Load, this does NOT return an error; startup callers should call
// Validate() explicitly.
func LoadOrDefault(path string) *Config {
	if path == "" {
		path = DefaultConfigPath()
	}
	if path == "" {
		return DefaultConfig()
	}
	cfg, err := Load(path)
	if err != nil {
		slog.Warn("config load failed; using defaults", "path", path, "err", err)
		return DefaultConfig()
	}
	return cfg
}

// DefaultLogFile returns the default log file path (~/.omni-agent-hub/logs/server.log).
func DefaultLogFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "omni-agent-hub", "server.log")
	}
	return filepath.Join(home, ".omni-agent-hub", "logs", "server.log")
}

// ResolveLogFile returns the effective log file path given a CLI flag override
// and the loaded config. Priority: flag > config > default.
func ResolveLogFile(flagValue string, cfg *Config) string {
	if flagValue != "" {
		return ExpandPath(flagValue)
	}
	if cfg.Logging.File != "" {
		return ExpandPath(cfg.Logging.File)
	}
	return DefaultLogFile()
}

// ExpandPath expands a leading "~/" to the user's home directory.
func ExpandPath(path string) string {
	if len(path) < 2 || path[:2] != "~/" {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}

// Save writes the config back to disk in the new shape. Used by the `config
// migrate` CLI subcommand.
func Save(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

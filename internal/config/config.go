// Package config loads aimon's YAML configuration and expands ${ENV} / $ENV
// references so secrets can be injected from the environment (12-factor /
// docker-compose friendly) without ever being committed.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level application configuration.
type Config struct {
	// Listen is the HTTP bind address, e.g. ":8080".
	Listen string `yaml:"listen"`
	// Database is the path to the SQLite file. The parent dir is created on boot.
	Database string `yaml:"database"`
	// PollInterval is how often every monitor is refreshed. Defaults to 60s.
	PollInterval time.Duration `yaml:"poll_interval"`
	// PublicURL is the externally reachable base URL, used to build OAuth
	// redirect URIs. Optional; falls back to the request host.
	PublicURL string `yaml:"public_url"`
	// Monitors is the flat list of accounts to watch.
	Monitors []Monitor `yaml:"monitors"`
}

// Monitor describes one account to poll. Only the fields relevant to the chosen
// provider are read; the rest may be left empty.
type Monitor struct {
	Provider string `yaml:"provider"` // deepseek | openrouter | anthropic | claude | xai
	Label    string `yaml:"label"`    // display name / account id (unique per provider)
	Enabled  *bool  `yaml:"enabled"`  // defaults to true when omitted

	// Credit/cost providers.
	APIKey   string `yaml:"api_key"`   // DeepSeek, OpenRouter, x.ai
	AdminKey string `yaml:"admin_key"` // Anthropic Admin API (sk-ant-admin-...)

	// Anthropic has no public prepaid-balance API. Set credit_balance to the
	// "credits left" shown in the Console and credit_as_of to the date you saw
	// it; aimon then reports remaining = credit_balance − spend since that date
	// (exact, from the cost report). Bump the baseline when you buy more credits.
	CreditBalance float64 `yaml:"credit_balance"`
	CreditAsOf    string  `yaml:"credit_as_of"` // "2006-01-02" or RFC3339

	// Subscription / OAuth providers.
	OAuth        bool   `yaml:"oauth"`         // use the interactive OAuth flow
	BearerToken  string `yaml:"bearer_token"`  // x.ai: pre-obtained session/bearer token
	TokenFile    string `yaml:"token_file"`    // x.ai: read a live token from a JSON file (e.g. ~/.hermes/auth.json)
	UsageURL     string `yaml:"usage_url"`     // override the usage endpoint
	AuthorizeURL string `yaml:"authorize_url"` // override the OAuth authorize host (claude/xai)
	BaseURL      string `yaml:"base_url"`      // override the API base (testing / proxies)
}

// IsEnabled reports whether the monitor should be polled (default true).
func (m Monitor) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }

var envRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expandEnv replaces ${VAR} and $VAR with the environment value (empty if unset).
func expandEnv(s string) string {
	return envRe.ReplaceAllStringFunc(s, func(m string) string {
		name := m[1:]
		if name[0] == '{' {
			name = name[1 : len(name)-1]
		}
		return os.Getenv(name)
	})
}

// Load reads, env-expands and validates the config at path, applying defaults.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	expanded := expandEnv(string(raw))

	var c Config
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	// Environment overrides win over the file — handy for containers where the
	// same config.yaml is bind-mounted but the DB must live under /data.
	if v := os.Getenv("AIMON_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("AIMON_DATABASE"); v != "" {
		c.Database = v
	}

	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.Database == "" {
		c.Database = "./data/aimon.db"
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 60 * time.Second
	}
	// Guard against hammering provider APIs.
	if c.PollInterval < 10*time.Second {
		c.PollInterval = 10 * time.Second
	}

	for i, m := range c.Monitors {
		if m.Provider == "" {
			return nil, fmt.Errorf("monitors[%d]: provider is required", i)
		}
		if m.Label == "" {
			// Default the label to the provider name so it's always addressable.
			c.Monitors[i].Label = m.Provider
		}
	}
	return &c, nil
}

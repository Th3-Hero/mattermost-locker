// Package config loads and validates the application configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // embed the tz database so "timezone" works in minimal containers

	"github.com/spf13/viper"
)

// Environment variables that may supply secrets instead of the config file.
// When set, they take precedence over the value in the file.
const (
	EnvToken      = "MATTERMOST_SLASH_COMMAND_TOKEN"
	EnvWebhookURL = "MATTERMOST_WEBHOOK_URL"
)

var lockNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// LockConfig describes one lockable resource.
type LockConfig struct {
	Name    string        `mapstructure:"name"`
	Timeout time.Duration `mapstructure:"timeout"`
}

// Config is the full application configuration.
type Config struct {
	WebserverPort       int           `mapstructure:"webserver_port"`
	SlashCommandToken   string        `mapstructure:"mattermost_slash_command_token"`
	WebhookURL          string        `mapstructure:"mattermost_webhook_url"`
	DBPath              string        `mapstructure:"db_path"`
	Timezone            string        `mapstructure:"timezone"`
	AuditDefaultLimit   int           `mapstructure:"audit_default_limit"`
	AuditMaxLimit       int           `mapstructure:"audit_max_limit"`
	ExpiryCheckInterval time.Duration `mapstructure:"expiry_check_interval"`
	Locks               []LockConfig  `mapstructure:"locks"`

	location *time.Location
}

// Location returns the validated display timezone.
func (c *Config) Location() *time.Location { return c.location }

// Load reads the JSON config file at path, applies defaults and environment
// overrides, and validates the result. Any problem is returned as an error;
// callers are expected to abort startup.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetConfigType("json")

	v.SetDefault("webserver_port", 8080)
	v.SetDefault("db_path", "locker.db")
	v.SetDefault("timezone", "UTC")
	v.SetDefault("audit_default_limit", 10)
	v.SetDefault("audit_max_limit", 100)
	v.SetDefault("expiry_check_interval", "5s")

	if err := v.BindEnv("mattermost_slash_command_token", EnvToken); err != nil {
		return nil, err
	}
	if err := v.BindEnv("mattermost_webhook_url", EnvWebhookURL); err != nil {
		return nil, err
	}

	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}

	var cfg Config
	// UnmarshalExact rejects unknown keys, which catches typos (and stale
	// keys such as the old mattermost_lock_channel_id).
	if err := v.UnmarshalExact(&cfg); err != nil {
		return nil, fmt.Errorf("parsing config %q: %w", path, err)
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %q:\n%w", path, err)
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.WebserverPort < 1 || c.WebserverPort > 65535 {
		add("webserver_port must be between 1 and 65535 (got %d)", c.WebserverPort)
	}

	switch {
	case strings.TrimSpace(c.SlashCommandToken) == "":
		add("mattermost_slash_command_token is required (config file or %s)", EnvToken)
	case strings.HasPrefix(c.SlashCommandToken, "YOUR_"):
		add("mattermost_slash_command_token still looks like a placeholder")
	}

	if c.WebhookURL == "" {
		add("mattermost_webhook_url is required (config file or %s)", EnvWebhookURL)
	} else if u, err := url.Parse(c.WebhookURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// Deliberately do not echo the value: webhook URLs contain a secret.
		add("mattermost_webhook_url must be a valid http(s) URL")
	} else if strings.Contains(c.WebhookURL, "YOUR_") {
		add("mattermost_webhook_url still looks like a placeholder")
	}

	if strings.TrimSpace(c.DBPath) == "" {
		add("db_path must not be empty")
	}

	if loc, err := time.LoadLocation(c.Timezone); err != nil {
		add("timezone %q is not a valid IANA timezone", c.Timezone)
	} else {
		c.location = loc
	}

	if c.AuditDefaultLimit < 1 {
		add("audit_default_limit must be at least 1 (got %d)", c.AuditDefaultLimit)
	}
	if c.AuditMaxLimit < 1 || c.AuditMaxLimit > 100 {
		add("audit_max_limit must be between 1 and 100 (got %d)", c.AuditMaxLimit)
	}
	if c.AuditDefaultLimit > c.AuditMaxLimit {
		add("audit_default_limit (%d) must not exceed audit_max_limit (%d)", c.AuditDefaultLimit, c.AuditMaxLimit)
	}

	if c.ExpiryCheckInterval < time.Second {
		add("expiry_check_interval must be at least 1s (got %s)", c.ExpiryCheckInterval)
	}

	if len(c.Locks) == 0 {
		add("at least one lock must be configured under \"locks\"")
	}
	seen := make(map[string]bool, len(c.Locks))
	for i, l := range c.Locks {
		switch {
		case !lockNameRE.MatchString(l.Name):
			add("locks[%d]: name %q is invalid (use letters, digits, '-' or '_', max 64 chars, starting with a letter or digit)", i, l.Name)
		case seen[strings.ToLower(l.Name)]:
			add("locks[%d]: duplicate lock name %q (names are case-insensitive)", i, l.Name)
		default:
			seen[strings.ToLower(l.Name)] = true
		}

		switch {
		case l.Timeout < time.Second:
			add("locks[%d] (%s): timeout must be a duration of at least 1s, written like \"24h\" or \"36m\" (got %s)", i, l.Name, l.Timeout)
		case l.Timeout%time.Second != 0:
			add("locks[%d] (%s): timeout must be a whole number of seconds (got %s)", i, l.Name, l.Timeout)
		}
	}

	return errors.Join(errs...)
}

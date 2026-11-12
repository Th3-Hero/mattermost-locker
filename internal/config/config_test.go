package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validJSON = `{
  "webserver_port": 9090,
  "mattermost_slash_command_token": "tok",
  "mattermost_webhook_url": "https://mm.example.com/hooks/abc",
  "locks": [
    {"name": "lock1", "timeout": "24h"},
    {"name": "lock2", "timeout": "36m"}
  ]
}`

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func clearEnv(t *testing.T) {
	t.Setenv(EnvToken, "")
	t.Setenv(EnvWebhookURL, "")
}

func TestLoadValidWithDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(write(t, validJSON))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebserverPort != 9090 || cfg.SlashCommandToken != "tok" {
		t.Errorf("unexpected values: %+v", cfg)
	}
	if cfg.DBPath != "locker.db" || cfg.AuditDefaultLimit != 10 || cfg.AuditMaxLimit != 100 ||
		cfg.ExpiryCheckInterval != 5*time.Second || cfg.Timezone != "UTC" {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if len(cfg.Locks) != 2 || cfg.Locks[0].Timeout != 24*time.Hour || cfg.Locks[1].Timeout != 36*time.Minute {
		t.Errorf("locks wrong: %+v", cfg.Locks)
	}
	if cfg.Location() == nil {
		t.Error("location not set")
	}
}

func TestEnvOverridesSecrets(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvToken, "from-env")
	t.Setenv(EnvWebhookURL, "https://other.example.com/hooks/zzz")
	cfg, err := Load(write(t, validJSON))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SlashCommandToken != "from-env" || cfg.WebhookURL != "https://other.example.com/hooks/zzz" {
		t.Errorf("env not applied: %q %q", cfg.SlashCommandToken, cfg.WebhookURL)
	}
}

func TestSecretsMayComeOnlyFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv(EnvToken, "from-env")
	t.Setenv(EnvWebhookURL, "https://mm.example.com/hooks/abc")
	_, err := Load(write(t, `{"locks":[{"name":"a","timeout":"1h"}]}`))
	if err != nil {
		t.Fatalf("expected success with secrets only in env: %v", err)
	}
}

func TestInvalidConfigs(t *testing.T) {
	cases := map[string]struct {
		json string
		want string
	}{
		"missing token":     {`{"mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"1h"}]}`, "mattermost_slash_command_token is required"},
		"placeholder token": {`{"mattermost_slash_command_token":"YOUR_MATTERMOST_TOKEN","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"1h"}]}`, "placeholder"},
		"missing webhook":   {`{"mattermost_slash_command_token":"t","locks":[{"name":"a","timeout":"1h"}]}`, "mattermost_webhook_url is required"},
		"bad webhook":       {`{"mattermost_slash_command_token":"t","mattermost_webhook_url":"nope","locks":[{"name":"a","timeout":"1h"}]}`, "valid http(s) URL"},
		"bad port":          {`{"webserver_port":70000,"mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"1h"}]}`, "webserver_port"},
		"no locks":          {`{"mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[]}`, "at least one lock"},
		"duplicate lock":    {`{"mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"1h"},{"name":"A","timeout":"1h"}]}`, "duplicate lock name"},
		"bad lock name":     {`{"mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"has space","timeout":"1h"}]}`, "invalid"},
		"bad timeout":       {`{"mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"soon"}]}`, ""},
		"numeric timeout":   {`{"mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":3600}]}`, "timeout must be a duration"},
		"zero timeout":      {`{"mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"0s"}]}`, "timeout must be a duration"},
		"subsecond timeout": {`{"mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"1500ms"}]}`, "whole number of seconds"},
		"bad timezone":      {`{"timezone":"Mars/Base","mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"1h"}]}`, "timezone"},
		"audit default>max": {`{"audit_default_limit":50,"audit_max_limit":20,"mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"1h"}]}`, "must not exceed"},
		"fast interval":     {`{"expiry_check_interval":"100ms","mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"1h"}]}`, "expiry_check_interval"},
		"unknown key":       {`{"mattermost_lock_channel_id":"x","mattermost_slash_command_token":"t","mattermost_webhook_url":"https://x/h","locks":[{"name":"a","timeout":"1h"}]}`, ""},
		"malformed json":    {`{`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			clearEnv(t)
			_, err := Load(write(t, tc.json))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	clearEnv(t)
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("expected error")
	}
}

func TestAllProblemsReportedAtOnce(t *testing.T) {
	clearEnv(t)
	_, err := Load(write(t, `{"webserver_port":0,"locks":[]}`))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"webserver_port", "mattermost_slash_command_token", "mattermost_webhook_url", "at least one lock"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %q", want, err)
		}
	}
}

func TestShippedConfigIsRejectedUntilEdited(t *testing.T) {
	clearEnv(t)
	// The sample config ships with placeholder secrets and must not start as-is.
	if _, err := Load("../../config.json"); err == nil {
		t.Fatal("sample config with placeholders should fail validation")
	}
}

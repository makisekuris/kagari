package config

import (
	"os"
	"testing"
	"time"
)

func TestPersonalDefaultsAndOwnerBoundary(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetLoggingEnv(t)
	unsetBrowserFallbackEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Storage.Path != "data/kagari-context.db" || cfg.Personal.Timezone != "Asia/Shanghai" || cfg.Personal.TaskConcurrency != 4 || cfg.Personal.TaskBudget != 30*time.Minute {
		t.Fatalf("personal defaults: %+v", cfg.Personal)
	}
	if cfg.Logging != (Logging{RotateMiB: 50, MaxTotalMiB: 1024, RetentionDays: 7}) {
		t.Fatalf("logging defaults: %+v", cfg.Logging)
	}
	if err := os.WriteFile("config.yaml", []byte("personal:\n  timezone: Europe/Berlin\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load("")
	if err != nil || cfg.Personal.Timezone != "Europe/Berlin" {
		t.Fatalf("personal.timezone from YAML: %q, %v", cfg.Personal.Timezone, err)
	}
	cfg.Model.Name, cfg.Model.APIKey, cfg.Model.BaseURL = "fixture", "test", "https://example.com/v1"
	cfg.Model.ContextWindow = 32768
	cfg.Telegram.Token, cfg.Telegram.AllowedUserIDs = "123:test", []int64{123}
	if err := cfg.ValidatePersonal(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Telegram.AllowedUserIDs = []int64{123, 456} },
		func(c *Config) { c.Personal.TaskConcurrency = 0 },
		func(c *Config) { c.Personal.TaskBudget = 0 },
		func(c *Config) { c.Personal.Timezone = "Mars/Olympus_Mons" },
		func(c *Config) { c.Personal.Timezone = "Local" },
	} {
		invalid := cfg
		change(&invalid)
		if invalid.ValidatePersonal() == nil {
			t.Fatal("invalid personal config accepted")
		}
	}
	t.Setenv("KAGARI_PERSONAL_TIMEZONE", "UTC")
	t.Setenv("KAGARI_PERSONAL_TASK_CONCURRENCY", "3")
	t.Setenv("KAGARI_PERSONAL_TASK_BUDGET", "5m")
	cfg, err = Load("")
	if err != nil || cfg.Personal.Timezone != "UTC" || cfg.Personal.TaskConcurrency != 3 || cfg.Personal.TaskBudget != 5*time.Minute {
		t.Fatalf("personal override: %+v, %v", cfg.Personal, err)
	}
	t.Setenv("KAGARI_PERSONAL_TIMEZONE", "Mars/Olympus_Mons")
	if _, err := Load(""); err == nil {
		t.Fatal("configuration loaded an invalid IANA time zone")
	}
}

func TestPersonalLoggingValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetLoggingEnv(t)
	unsetBrowserFallbackEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.Name, cfg.Model.APIKey, cfg.Model.BaseURL = "fixture", "test", "https://example.com/v1"
	cfg.Model.ContextWindow = 32768
	cfg.Telegram.Token, cfg.Telegram.AllowedUserIDs = "123:test", []int64{123}
	for name, change := range map[string]func(*Config){
		"zero rotation":                func(c *Config) { c.Logging.RotateMiB = 0 },
		"zero total":                   func(c *Config) { c.Logging.MaxTotalMiB = 0 },
		"rotation exceeds total":       func(c *Config) { c.Logging.RotateMiB = c.Logging.MaxTotalMiB + 1 },
		"MiB conversion overflow":      func(c *Config) { c.Logging.MaxTotalMiB = maxLoggingSizeMiB + 1 },
		"duration conversion overflow": func(c *Config) { c.Logging.RetentionDays = int(maxLoggingRetentionDays + 1) },
	} {
		invalid := cfg
		change(&invalid)
		if err := invalid.ValidatePersonal(); err == nil {
			t.Errorf("accepted invalid logging config: %s", name)
		}
	}
	cfg.Logging = Logging{RotateMiB: maxLoggingSizeMiB, MaxTotalMiB: maxLoggingSizeMiB, RetentionDays: int(maxLoggingRetentionDays)}
	if err := cfg.ValidatePersonal(); err != nil {
		t.Fatalf("accepted maximum safely convertible logging config: %v", err)
	}
}

func TestWeixinConfigDoesNotRequireTelegram(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetLoggingEnv(t)
	unsetBrowserFallbackEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Model.Name, cfg.Model.APIKey, cfg.Model.BaseURL = "fixture", "test", "https://example.com/v1"
	cfg.Model.ContextWindow = 32768
	cfg.Personal.Channel = "weixin"
	cfg.Telegram = Telegram{}
	if err := cfg.ValidatePersonal(); err != nil {
		t.Fatal(err)
	}
	cfg.Personal.Channel = "unknown"
	if err := cfg.ValidatePersonal(); err == nil {
		t.Fatal("unknown channel accepted")
	}
}

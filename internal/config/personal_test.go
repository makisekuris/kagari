package config

import (
	"testing"
	"time"
)

func TestPersonalDefaultsAndOwnerBoundary(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Personal.TaskConcurrency != 4 || cfg.Personal.TaskBudget != 30*time.Minute {
		t.Fatalf("personal defaults: %+v", cfg.Personal)
	}
	cfg.Model.Name, cfg.Model.APIKey, cfg.Model.BaseURL = "fixture", "test", "https://example.com/v1"
	cfg.Telegram.Token, cfg.Telegram.AllowedUserIDs = "123:test", []int64{123}
	if err := cfg.ValidatePersonal(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Telegram.AllowedUserIDs = []int64{123, 456} },
		func(c *Config) { c.Telegram.TargetChatIDs = []int64{123} },
		func(c *Config) { c.Personal.TaskConcurrency = 0 },
		func(c *Config) { c.Personal.TaskBudget = 0 },
	} {
		invalid := cfg
		change(&invalid)
		if invalid.ValidatePersonal() == nil {
			t.Fatal("invalid personal config accepted")
		}
	}
	t.Setenv("KAGARI_PERSONAL_TASK_CONCURRENCY", "3")
	t.Setenv("KAGARI_PERSONAL_TASK_BUDGET", "5m")
	cfg, err = Load("")
	if err != nil || cfg.Personal.TaskConcurrency != 3 || cfg.Personal.TaskBudget != 5*time.Minute {
		t.Fatalf("personal override: %+v, %v", cfg.Personal, err)
	}
}

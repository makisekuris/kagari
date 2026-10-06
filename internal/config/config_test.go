package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigEnvironmentAndValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetBrowserFallbackEnv(t)
	t.Setenv("KAGARI_MODEL_API_KEY", "secret")
	t.Setenv("KAGARI_TELEGRAM_ALLOWED_USER_IDS", "123,456")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model.APIKey != "secret" || len(c.Telegram.AllowedUserIDs) != 2 {
		t.Fatalf("environment not loaded: %+v", c.Telegram)
	}
	if c.Browser.Enabled || c.Weekly.Enabled {
		t.Fatal("optional integrations enabled by default")
	}
	if c.MaxAttempts != 6 {
		t.Fatalf("max_attempts default = %d, want 6 total attempts", c.MaxAttempts)
	}
	if err := c.Validate(true, true); err == nil {
		t.Fatal("missing model endpoint accepted")
	}
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("reader:\n  browser_fallback:\n    enabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("removed browser_fallback configuration accepted")
	}
	if err := os.WriteFile(path, []byte("reader:\n  typo: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown config accepted")
	}
}

func TestAgentStreamingOverride(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("config.yaml", []byte("agent:\n  streaming: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KAGARI_AGENT_STREAMING", "false")
	c, err := Load("")
	if err != nil || c.Agent.Streaming {
		t.Fatalf("environment did not disable streaming: %v %v", c.Agent.Streaming, err)
	}
	t.Setenv("KAGARI_AGENT_STREAMING", "true")
	c, err = Load("")
	if err != nil || !c.Agent.Streaming {
		t.Fatalf("environment did not enable streaming: %v %v", c.Agent.Streaming, err)
	}
}

func TestDotEnvPrecedenceAndIsolation(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetBrowserFallbackEnv(t)
	t.Setenv("KAGARI_MODEL_NAME", "system-model")
	t.Setenv("KAGARI_MODEL_API_KEY", "")
	t.Setenv("KAGARI_TELEGRAM_TOKEN", "system-token")
	// 其余变量应由 .env 读取，避免本机的测试环境干扰优先级检查。
	for _, key := range []string{"KAGARI_MODEL_BASE_URL", "KAGARI_TELEGRAM_ALLOWED_USER_IDS", "KAGARI_LOG_LEVEL"} {
		value, present := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
	if err := os.WriteFile("config.yaml", []byte("model:\n  name: yaml-model\n  base_url: https://yaml.example/v1\nlog_level: debug\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dotenv := "KAGARI_MODEL_NAME=dotenv-model\nKAGARI_MODEL_BASE_URL=https://dotenv.example/v1\nKAGARI_MODEL_API_KEY='dotenv-secret'\nKAGARI_TELEGRAM_TOKEN='dotenv-token'\nKAGARI_TELEGRAM_ALLOWED_USER_IDS=123,456\n"
	if err := os.WriteFile(".env", []byte(dotenv), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model.Name != "system-model" || c.Model.BaseURL != "https://dotenv.example/v1" || c.Model.APIKey != "" || c.Telegram.Token != "system-token" {
		t.Fatal("system/.env/YAML precedence is incorrect")
	}
	if len(c.Telegram.AllowedUserIDs) != 2 || c.Telegram.AllowedUserIDs[1] != 456 || c.LogLevel != "debug" {
		t.Fatal("dotenv lists or YAML fallback not loaded")
	}
	if _, present := os.LookupEnv("KAGARI_MODEL_BASE_URL"); present {
		t.Fatal("dotenv modified process environment")
	}
	if err := os.Remove(".env"); err != nil {
		t.Fatal(err)
	}
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model.BaseURL != "https://yaml.example/v1" {
		t.Fatal("dotenv value leaked into a later config load")
	}
	if err := os.WriteFile(".env", []byte("not-valid secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = Load("")
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatal("invalid dotenv accepted or credential exposed")
	}
}

func TestExampleDotEnvCanStartLocalCommands(t *testing.T) {
	// 只读取可提交的示例，不接触开发者的真实 .env。
	dotenvTemplate, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	configTemplate, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".env", dotenvTemplate, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("config.yaml", configTemplate, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(""); err != nil {
		t.Fatalf("example dotenv and YAML cannot load for read/digest: %v", err)
	}
}

func TestBrowserPolicyConfiguration(t *testing.T) {
	cfg := validConfigForTest()
	cfg.Browser.Enabled = true
	cfg.Browser.Command = ""
	if cfg.Validate(false, false) == nil {
		t.Fatal("missing command accepted")
	}
	cfg.Browser.Command = "npx"
	cfg.Browser.Args = []string{"--cdp-endpoint=http://localhost:9222"}
	if cfg.Validate(false, false) == nil {
		t.Fatal("network policy bypass accepted")
	}
	cfg.Browser.Args = []string{"@playwright/mcp@0.0.82", "--headless"}
	if err := cfg.Validate(false, false); err != nil {
		t.Fatal(err)
	}
}

func TestAllowedNonPublicCIDRsEnvironmentAndValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetBrowserFallbackEnv(t)
	cfg, err := Load("")
	if err != nil || len(cfg.Reader.AllowedNonPublicCIDRs) != 0 {
		t.Fatalf("default allowed CIDRs = %v, err=%v; want empty", cfg.Reader.AllowedNonPublicCIDRs, err)
	}
	t.Setenv("KAGARI_READER_ALLOWED_NON_PUBLIC_CIDRS", "198.18.0.0/16,fd00::/8")
	cfg, err = Load("")
	if err != nil || len(cfg.Reader.AllowedNonPublicCIDRs) != 2 || cfg.Reader.AllowedNonPublicCIDRs[0] != "198.18.0.0/16" || cfg.Reader.AllowedNonPublicCIDRs[1] != "fd00::/8" {
		t.Fatalf("allowed CIDRs from env = %v, err=%v", cfg.Reader.AllowedNonPublicCIDRs, err)
	}

	for _, cidr := range []string{"198.18.0.1", "198.18.0.0/33", "::ffff:198.18.0.0/112", "fe80::1%eth0/64"} {
		cfg := validConfigForTest()
		cfg.Reader.AllowedNonPublicCIDRs = []string{cidr}
		if err := cfg.Validate(false, false); err == nil || !strings.Contains(err.Error(), "allowed_non_public_cidrs") {
			t.Errorf("Validate() accepted %q or returned an unrelated error: %v", cidr, err)
		}
	}
	cfg = validConfigForTest()
	cfg.Reader.AllowedNonPublicCIDRs = []string{"198.18.0.0/16", "fd00::/8"}
	if err := cfg.Validate(false, false); err != nil {
		t.Fatalf("valid allowed CIDRs rejected: %v", err)
	}
	t.Setenv("KAGARI_READER_ALLOWED_NON_PUBLIC_CIDRS", "not-a-cidr")
	if _, err := Load(""); err == nil {
		t.Fatal("invalid allowed CIDR from environment was accepted")
	}
}

func unsetBrowserFallbackEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"KAGARI_READER_ALLOWED_NON_PUBLIC_CIDRS",
		"KAGARI_READER_BROWSER_FALLBACK_ENABLED",
		"KAGARI_READER_BROWSER_FALLBACK_ENGINE",
		"KAGARI_READER_BROWSER_FALLBACK_MODE",
		"KAGARI_READER_BROWSER_FALLBACK_EXECUTABLE_PATH",
		"KAGARI_READER_BROWSER_FALLBACK_ENDPOINT",
		"KAGARI_READER_BROWSER_FALLBACK_HEADLESS",
	} {
		value, present := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		key := key
		t.Cleanup(func() {
			if present {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func validConfigForTest() Config {
	var c Config
	c.Storage.Path = "data/kagari.db"
	c.Model.Timeout = time.Minute
	c.Model.MaxOutputTokens = 5000
	c.Reader.Timeout = 20 * time.Second
	c.Reader.MaxBytes = 4 << 20
	c.Reader.MaxContentChars = 20_000
	c.Reader.MaxLinks = 40
	c.Reader.CacheTTL = 24 * time.Hour
	c.Agent.MaxSources = 6
	c.Agent.MaxIterations = 10
	c.Agent.Timeout = 3 * time.Minute
	c.Weekly.MaxInputChars = DefaultWeeklyInputChars
	c.Weekly.Timezone = "UTC"
	c.Weekly.Time = "09:00"
	c.MaxAttempts = 3
	return c
}

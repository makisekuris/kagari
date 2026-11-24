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
	if c.Browser.Enabled {
		t.Fatal("optional integrations enabled by default")
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

func TestModelAPITypeDefaultsAndYAML(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetModelAPITypeEnv(t)
	for _, tc := range []struct {
		name string
		yaml string
		want ModelAPIType
	}{
		{name: "omitted", want: ModelAPIResponses},
		{name: "empty", yaml: "model:\n  apiType: \"\"\n", want: ModelAPIResponses},
		{name: "null", yaml: "model:\n  apiType: null\n", want: ModelAPIResponses},
		{name: "configured", yaml: "model:\n  apiType: chat_completions\n", want: ModelAPIChatCompletions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile("config.yaml", []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load("")
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Model.EffectiveAPIType(); got != tc.want {
				t.Fatalf("EffectiveAPIType() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestModelAPITypeEnvironmentPrecedenceAndValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetModelAPITypeEnv(t)
	if err := os.WriteFile("config.yaml", []byte("model:\n  apiType: chat_completions\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(".env", []byte("KAGARI_MODEL_API_TYPE=responses\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil || cfg.Model.APIType != ModelAPIResponses {
		t.Fatalf(".env did not override YAML: apiType=%q err=%v", cfg.Model.APIType, err)
	}
	t.Setenv("KAGARI_MODEL_API_TYPE", "chat_completions")
	cfg, err = Load("")
	if err != nil || cfg.Model.APIType != ModelAPIChatCompletions {
		t.Fatalf("system environment did not override .env: apiType=%q err=%v", cfg.Model.APIType, err)
	}
	t.Setenv("KAGARI_MODEL_API_TYPE", "")
	cfg, err = Load("")
	if err != nil || cfg.Model.APIType != "" || cfg.Model.EffectiveAPIType() != ModelAPIResponses {
		t.Fatalf("explicit empty system environment did not override lower precedence: apiType=%q effective=%q err=%v", cfg.Model.APIType, cfg.Model.EffectiveAPIType(), err)
	}
	t.Setenv("KAGARI_MODEL_API_TYPE", "chat-completions")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "model.apiType") {
		t.Fatalf("invalid API type accepted or not identified: %v", err)
	}
}

func unsetModelAPITypeEnv(t *testing.T) {
	t.Helper()
	key := "KAGARI_MODEL_API_TYPE"
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

func TestOptionalMultimodalModelConfiguration(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetMultimodalModelEnv(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MultimodalModel != nil {
		t.Fatalf("omitted multimodal model = %+v, want nil", cfg.MultimodalModel)
	}
	for _, yaml := range []string{"multimodal_model: {}\n", "multimodal_model: null\n"} {
		if err := os.WriteFile("config.yaml", []byte(yaml), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err = Load("")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MultimodalModel != nil {
			t.Fatalf("empty multimodal block %q = %+v, want nil", yaml, cfg.MultimodalModel)
		}
	}

	if err := os.WriteFile("config.yaml", []byte("multimodal_model:\n  base_url: https://vision.example/v1\n  name: vision-model\n  api_key: vision-secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	model := cfg.MultimodalModel
	if model == nil || model.BaseURL != "https://vision.example/v1" || model.Name != "vision-model" || model.APIKey != "vision-secret" {
		t.Fatalf("multimodal model fields = %+v", model)
	}
	if model.EffectiveAPIType() != ModelAPIResponses || model.Timeout != time.Minute || model.MaxOutputTokens != 5000 {
		t.Fatalf("multimodal defaults = apiType %q, timeout %s, max output %d", model.EffectiveAPIType(), model.Timeout, model.MaxOutputTokens)
	}
	if model.ContextWindow != 0 {
		t.Fatalf("multimodal context window = %d, want unset", model.ContextWindow)
	}
	if cfg.Model.BaseURL != "" || cfg.Model.Name != "" || cfg.Model.APIKey != "" {
		t.Fatalf("primary model unexpectedly inherited multimodal settings: %+v", cfg.Model)
	}
}

func TestMultimodalModelEnvironmentPrecedenceAndValidation(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetMultimodalModelEnv(t)
	configYAML := "multimodal_model:\n  apiType: chat_completions\n  base_url: https://yaml.example/v1\n  name: yaml-model\n  api_key: yaml-key\n  timeout: 20s\n  max_output_tokens: 256\n"
	if err := os.WriteFile("config.yaml", []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}
	dotenv := "KAGARI_MULTIMODAL_MODEL_BASE_URL=https://dotenv.example/v1\nKAGARI_MULTIMODAL_MODEL_NAME=dotenv-model\nKAGARI_MULTIMODAL_MODEL_API_KEY=dotenv-key\nKAGARI_MULTIMODAL_MODEL_API_TYPE=responses\nKAGARI_MULTIMODAL_MODEL_TIMEOUT=45s\nKAGARI_MULTIMODAL_MODEL_MAX_OUTPUT_TOKENS=1024\n"
	if err := os.WriteFile(".env", []byte(dotenv), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	model := cfg.MultimodalModel
	if model == nil || model.BaseURL != "https://dotenv.example/v1" || model.Name != "dotenv-model" || model.APIKey != "dotenv-key" || model.APIType != ModelAPIResponses || model.Timeout != 45*time.Second || model.MaxOutputTokens != 1024 {
		t.Fatalf("multimodal .env values = %+v", model)
	}
	t.Setenv("KAGARI_MULTIMODAL_MODEL_BASE_URL", "https://system.example/v1")
	t.Setenv("KAGARI_MULTIMODAL_MODEL_API_TYPE", "chat_completions")
	cfg, err = Load("")
	if err != nil || cfg.MultimodalModel == nil || cfg.MultimodalModel.BaseURL != "https://system.example/v1" || cfg.MultimodalModel.APIType != ModelAPIChatCompletions {
		t.Fatalf("system environment did not override .env: model=%+v err=%v", cfg.MultimodalModel, err)
	}

	t.Setenv("KAGARI_MULTIMODAL_MODEL_API_TYPE", "chat-completions")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "multimodal_model.apiType") {
		t.Fatalf("invalid multimodal API type accepted or not identified: %v", err)
	}
	t.Setenv("KAGARI_MULTIMODAL_MODEL_API_TYPE", "responses")
	t.Setenv("KAGARI_MULTIMODAL_MODEL_TIMEOUT", "0s")
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "multimodal_model.timeout") {
		t.Fatalf("invalid multimodal timeout accepted or not identified: %v", err)
	}
}

func TestIncompleteMultimodalModelIsRejected(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetMultimodalModelEnv(t)
	configYAML := "model:\n  base_url: https://text.example/v1\n  name: text-model\n  api_key: text-key\n  context_window: 32768\nmultimodal_model:\n  api_key: only-a-key\n"
	if err := os.WriteFile("config.yaml", []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("read-only config load rejected missing multimodal endpoint: %v", err)
	}
	if err := cfg.Validate(false, false); err != nil {
		t.Fatalf("local validation required multimodal credentials: %v", err)
	}
	if err := cfg.Validate(true, false); err == nil || !strings.Contains(err.Error(), "multimodal_model.base_url, multimodal_model.name and multimodal_model.api_key are required") {
		t.Fatalf("incomplete multimodal model accepted or not identified: %v", err)
	}
}

func unsetMultimodalModelEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"KAGARI_MULTIMODAL_MODEL_BASE_URL", "KAGARI_MULTIMODAL_MODEL_NAME", "KAGARI_MULTIMODAL_MODEL_API_KEY",
		"KAGARI_MULTIMODAL_MODEL_API_TYPE", "KAGARI_MULTIMODAL_MODEL_TIMEOUT", "KAGARI_MULTIMODAL_MODEL_MAX_OUTPUT_TOKENS",
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

func TestRemovedArchiveSettingsAreRejected(t *testing.T) {
	t.Chdir(t.TempDir())
	for name, yaml := range map[string]string{
		"weekly":          "weekly:\n  enabled: false\n",
		"max_attempts":    "max_attempts: 6\n",
		"cache_ttl":       "reader:\n  cache_ttl: 24h\n",
		"target_chat_ids": "telegram:\n  target_chat_ids: []\n",
	} {
		if err := os.WriteFile("config.yaml", []byte(yaml), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(""); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("removed setting %s: got %v", name, err)
		}
	}
}

func TestLoggingDefaultsAndEnvironment(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetLoggingEnv(t)
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Logging != (Logging{RotateMiB: 50, MaxTotalMiB: 1024, RetentionDays: 7}) {
		t.Fatalf("logging defaults = %+v", cfg.Logging)
	}
	t.Setenv("KAGARI_LOGGING_DIRECTORY", "var/kagari.logs")
	t.Setenv("KAGARI_LOGGING_ROTATE_MIB", "25")
	t.Setenv("KAGARI_LOGGING_MAX_TOTAL_MIB", "2048")
	t.Setenv("KAGARI_LOGGING_RETENTION_DAYS", "30")
	cfg, err = Load("")
	if err != nil || cfg.Logging != (Logging{Directory: "var/kagari.logs", RotateMiB: 25, MaxTotalMiB: 2048, RetentionDays: 30}) {
		t.Fatalf("logging environment = %+v, err=%v", cfg.Logging, err)
	}
}

func TestLocalValidationDoesNotRequireServiceLoggingConfig(t *testing.T) {
	if err := validConfigForTest().Validate(false, false); err != nil {
		t.Fatalf("local validation rejected a config without logging fields: %v", err)
	}
}

func TestContextWindowRequiredOnlyForModelCalls(t *testing.T) {
	cfg := validConfigForTest()
	cfg.Model.ContextWindow = 0
	if err := cfg.Validate(false, false); err != nil {
		t.Fatalf("local validation requires a model context window: %v", err)
	}
	if err := cfg.Validate(true, false); err == nil || !strings.Contains(err.Error(), "model.context_window") {
		t.Fatalf("model validation did not identify missing context window: %v", err)
	}
}

func TestDotEnvPrecedenceAndIsolation(t *testing.T) {
	t.Chdir(t.TempDir())
	unsetBrowserFallbackEnv(t)
	t.Setenv("KAGARI_MODEL_NAME", "system-model")
	t.Setenv("KAGARI_MODEL_API_KEY", "")
	t.Setenv("KAGARI_TELEGRAM_TOKEN", "system-token")
	// 其余变量应由 .env 读取，避免本机的测试环境干扰优先级检查。
	for _, key := range []string{"KAGARI_MODEL_BASE_URL", "KAGARI_MODEL_CONTEXT_WINDOW", "KAGARI_TELEGRAM_ALLOWED_USER_IDS", "KAGARI_LOG_LEVEL"} {
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
	dotenv := "KAGARI_MODEL_NAME=dotenv-model\nKAGARI_MODEL_BASE_URL=https://dotenv.example/v1\nKAGARI_MODEL_CONTEXT_WINDOW=32768\nKAGARI_MODEL_API_KEY='dotenv-secret'\nKAGARI_TELEGRAM_TOKEN='dotenv-token'\nKAGARI_TELEGRAM_ALLOWED_USER_IDS=123,456\n"
	if err := os.WriteFile(".env", []byte(dotenv), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Model.Name != "system-model" || c.Model.BaseURL != "https://dotenv.example/v1" || c.Model.ContextWindow != 32768 || c.Model.APIKey != "" || c.Telegram.Token != "system-token" {
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
	if c.Model.BaseURL != "https://yaml.example/v1" || c.Model.ContextWindow != 0 {
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
		t.Fatalf("example dotenv and YAML cannot load for local inspection: %v", err)
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
		"KAGARI_MODEL_CONTEXT_WINDOW",
		"KAGARI_PERSONAL_TIMEZONE",
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

func unsetLoggingEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"KAGARI_LOGGING_DIRECTORY", "KAGARI_LOGGING_ROTATE_MIB", "KAGARI_LOGGING_MAX_TOTAL_MIB", "KAGARI_LOGGING_RETENTION_DAYS"} {
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
	c.Storage.Path = "data/kagari-context.db"
	c.Personal.Timezone = "Asia/Shanghai"
	c.Model.Timeout = time.Minute
	c.Model.MaxOutputTokens = 5000
	c.Model.ContextWindow = 32768
	c.Reader.Timeout = 20 * time.Second
	c.Reader.MaxBytes = 4 << 20
	c.Reader.MaxContentChars = 20_000
	c.Reader.MaxLinks = 40
	c.Agent.MaxSources = 6
	c.Agent.MaxIterations = 10
	c.Agent.Timeout = 3 * time.Minute
	return c
}

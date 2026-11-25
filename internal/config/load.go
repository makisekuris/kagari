package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/subosito/gotenv"
)

const (
	defaultModelTimeout         = 60 * time.Second
	defaultModelMaxOutputTokens = 5000
	defaultArchiveRegion        = "us-east-1"
)

// Load 从当前工作目录读取可选的 .env 和 config.yaml；path 只替换 YAML 路径。
// 优先级为系统环境变量 > .env > YAML > 默认值，加载不修改进程的环境变量。
func Load(path string) (Config, error) {
	env, err := readDotEnv()
	if err != nil {
		return Config{}, err
	}
	v := viper.New()
	defaults := map[string]any{
		"storage.path": "data/kagari-context.db", "profile_path": "profile.md", "log_level": "info",
		"model.context_window": int64(0), "model.timeout": defaultModelTimeout, "model.max_output_tokens": defaultModelMaxOutputTokens,
		"reader.timeout": "20s", "reader.max_bytes": 4 << 20, "reader.max_content_chars": 20000,
		"reader.max_links": 40, "reader.allowed_non_public_cidrs": []string{},
		"agent.max_sources": 6,
		"personal.channel":  "telegram", "personal.timezone": "Asia/Shanghai", "personal.task_concurrency": 4, "personal.task_budget": "30m",
		"logging.directory": "", "logging.rotate_mib": 50, "logging.max_total_mib": 1024, "logging.retention_days": 7,
		"browser.enabled": false, "browser.command": "npx", "browser.args": []string{"-y", "@playwright/mcp@0.0.82", "--headless", "--isolated", "--codegen", "none"},
		"agent.max_iterations": 10, "agent.timeout": "3m",
		"agent.streaming": false,
		"archive.enabled": false, "archive.endpoint": "", "archive.bucket": "", "archive.region": defaultArchiveRegion,
		"archive.access_key_id": "", "archive.secret_access_key": "",
	}
	for key, value := range defaults {
		v.SetDefault(key, value)
	}
	multimodalKeys := []string{
		"multimodal_model.base_url", "multimodal_model.api_key", "multimodal_model.apiType", "multimodal_model.name",
		"multimodal_model.timeout", "multimodal_model.max_output_tokens",
	}
	keys := []string{
		"model.base_url", "model.api_key", "model.apiType", "model.name",
		"telegram.token", "telegram.allowed_user_ids",
	}
	keys = append(keys, multimodalKeys...)
	for key := range defaults {
		keys = append(keys, key)
	}
	for _, key := range keys {
		envKey := strings.ReplaceAll(key, "apiType", "api_type")
		name := "KAGARI_" + strings.ToUpper(strings.ReplaceAll(envKey, ".", "_"))
		if err := v.BindEnv(key, name); err != nil {
			return Config{}, err
		}
		// 只接受已知配置键；即使系统变量显式为空，也不能用 .env 中的凭证覆盖它。
		if value, present := os.LookupEnv(name); present {
			v.Set(key, value)
		} else if value, present := env[name]; present {
			v.Set(key, value)
		}
	}
	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return Config{}, fmt.Errorf("read config: %w", err)
		}
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		if err := v.ReadInConfig(); err != nil {
			var missing viper.ConfigFileNotFoundError
			if !errors.As(err, &missing) {
				return Config{}, fmt.Errorf("read config: %w", err)
			}
		}
	}
	configured := slices.ContainsFunc(multimodalKeys, v.IsSet)
	if configured {
		v.SetDefault("multimodal_model.timeout", defaultModelTimeout)
		v.SetDefault("multimodal_model.max_output_tokens", defaultModelMaxOutputTokens)
	}
	var cfg Config
	if err := v.UnmarshalExact(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
	}
	if !configured {
		cfg.MultimodalModel = nil
	}
	if cfg.Archive.Region == "" {
		cfg.Archive.Region = defaultArchiveRegion
	}
	return cfg, cfg.Validate(false, false)
}

func readDotEnv() (gotenv.Env, error) {
	f, err := os.Open(".env")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read .env: %w", err)
	}
	defer f.Close()
	env, err := gotenv.StrictParse(f)
	if err != nil {
		// dotenv 的解析错误可能包含出错行；启动诊断不能泄漏其中的密钥。
		return nil, errors.New("parse .env: invalid dotenv syntax; check the local file")
	}
	return env, nil
}

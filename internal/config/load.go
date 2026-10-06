package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
	"github.com/subosito/gotenv"
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
		"storage.path": "data/kagari.db", "profile_path": "profile.md", "log_level": "info", "max_attempts": 6,
		"model.timeout": "60s", "model.max_output_tokens": 5000,
		"reader.timeout": "20s", "reader.max_bytes": 4 << 20, "reader.max_content_chars": 20000,
		"reader.max_links": 40, "reader.cache_ttl": "24h", "reader.allowed_non_public_cidrs": []string{},
		"agent.max_sources": 6,
		"browser.enabled":   false, "browser.command": "npx", "browser.args": []string{"-y", "@playwright/mcp@0.0.82", "--headless", "--isolated", "--codegen", "none"},
		"agent.max_iterations": 10, "agent.timeout": "3m",
		"agent.streaming": false,
		"weekly.enabled":  false, "weekly.timezone": "Asia/Shanghai", "weekly.weekday": 1,
		"weekly.time": "09:00", "weekly.max_input_chars": DefaultWeeklyInputChars,
	}
	for key, value := range defaults {
		v.SetDefault(key, value)
	}
	keys := []string{"model.base_url", "model.api_key", "model.name", "telegram.token", "telegram.allowed_user_ids", "telegram.target_chat_ids"}
	for key := range defaults {
		keys = append(keys, key)
	}
	for _, key := range keys {
		name := "KAGARI_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
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
	var cfg Config
	if err := v.UnmarshalExact(&cfg); err != nil {
		return cfg, fmt.Errorf("decode config: %w", err)
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

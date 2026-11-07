package config

import "time"

type Config struct {
	Model   Model `mapstructure:"model"`
	Storage struct {
		Path string `mapstructure:"path"`
	} `mapstructure:"storage"`
	Telegram    Telegram `mapstructure:"telegram"`
	Reader      Reader   `mapstructure:"reader"`
	Browser     Browser  `mapstructure:"browser"`
	Agent       Agent    `mapstructure:"agent"`
	Personal    Personal `mapstructure:"personal"`
	Logging     Logging  `mapstructure:"logging"`
	ProfilePath string   `mapstructure:"profile_path"`
	LogLevel    string   `mapstructure:"log_level"`
}

type Model struct {
	BaseURL         string        `mapstructure:"base_url"`
	APIKey          string        `mapstructure:"api_key"`
	Name            string        `mapstructure:"name"`
	ContextWindow   int64         `mapstructure:"context_window"`
	Timeout         time.Duration `mapstructure:"timeout"`
	MaxOutputTokens int           `mapstructure:"max_output_tokens"`
}

type Personal struct {
	Channel         string        `mapstructure:"channel"`
	Timezone        string        `mapstructure:"timezone"`
	TaskConcurrency int           `mapstructure:"task_concurrency"`
	TaskBudget      time.Duration `mapstructure:"task_budget"`
}

type Telegram struct {
	Token          string  `mapstructure:"token"`
	AllowedUserIDs []int64 `mapstructure:"allowed_user_ids"`
}

type Reader struct {
	Timeout               time.Duration `mapstructure:"timeout"`
	MaxBytes              int64         `mapstructure:"max_bytes"`
	MaxContentChars       int           `mapstructure:"max_content_chars"`
	MaxLinks              int           `mapstructure:"max_links"`
	AllowedNonPublicCIDRs []string      `mapstructure:"allowed_non_public_cidrs"`
}

// Browser 启动独立的 Playwright MCP 会话；HTTP 读取和浏览器读取由模型选择。
type Browser struct {
	Enabled bool     `mapstructure:"enabled"`
	Command string   `mapstructure:"command"`
	Args    []string `mapstructure:"args"`
}

type Agent struct {
	MaxSources    int           `mapstructure:"max_sources"` // 包括入口、原文、补读和失败尝试。
	MaxIterations int           `mapstructure:"max_iterations"`
	Timeout       time.Duration `mapstructure:"timeout"`
	Streaming     bool          `mapstructure:"streaming"`
}

type Logging struct {
	Directory     string `mapstructure:"directory"`
	RotateMiB     int64  `mapstructure:"rotate_mib"`
	MaxTotalMiB   int64  `mapstructure:"max_total_mib"`
	RetentionDays int    `mapstructure:"retention_days"`
}

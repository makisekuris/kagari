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
	Weekly      Weekly   `mapstructure:"weekly"`
	ProfilePath string   `mapstructure:"profile_path"`
	LogLevel    string   `mapstructure:"log_level"`
	MaxAttempts int      `mapstructure:"max_attempts"` // 包含首次尝试；处理任务和已知未发送的投递共用上限。
}

type Model struct {
	BaseURL         string        `mapstructure:"base_url"`
	APIKey          string        `mapstructure:"api_key"`
	Name            string        `mapstructure:"name"`
	Timeout         time.Duration `mapstructure:"timeout"`
	MaxOutputTokens int           `mapstructure:"max_output_tokens"`
}

type Telegram struct {
	Token          string  `mapstructure:"token"`
	AllowedUserIDs []int64 `mapstructure:"allowed_user_ids"`
	TargetChatID   int64   `mapstructure:"target_chat_id"`
	TargetChatIDs  []int64 `mapstructure:"target_chat_ids"`
}

// ChatIDs 优先使用多目标配置；未配置时兼容单目标或回复提交私聊。
func (t Telegram) ChatIDs(fallback int64) []int64 {
	if len(t.TargetChatIDs) > 0 {
		return append([]int64(nil), t.TargetChatIDs...)
	}
	if t.TargetChatID != 0 {
		return []int64{t.TargetChatID}
	}
	if fallback != 0 {
		return []int64{fallback}
	}
	return nil
}

type Reader struct {
	Timeout               time.Duration `mapstructure:"timeout"`
	MaxBytes              int64         `mapstructure:"max_bytes"`
	MaxContentChars       int           `mapstructure:"max_content_chars"`
	MaxLinks              int           `mapstructure:"max_links"`
	CacheTTL              time.Duration `mapstructure:"cache_ttl"` // 来源缓存与已完成分析的复用时限；0 禁用复用。
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

type Weekly struct {
	Enabled       bool   `mapstructure:"enabled"`
	Timezone      string `mapstructure:"timezone"`
	Weekday       int    `mapstructure:"weekday"`
	Time          string `mapstructure:"time"`
	MaxInputChars int    `mapstructure:"max_input_chars"`
}

const DefaultWeeklyInputChars = 120000

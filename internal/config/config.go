package config

import "time"

type Config struct {
	Model   Model `mapstructure:"model"`
	Storage struct {
		Path string `mapstructure:"path"`
	} `mapstructure:"storage"`
	Telegram    Telegram `mapstructure:"telegram"`
	Reader      Reader   `mapstructure:"reader"`
	Agent       Agent    `mapstructure:"agent"`
	Weekly      Weekly   `mapstructure:"weekly"`
	ProfilePath string   `mapstructure:"profile_path"`
	LogLevel    string   `mapstructure:"log_level"`
	MaxAttempts int      `mapstructure:"max_attempts"` // 处理任务和已知未发送的投递共用尝试上限。
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
}

type Reader struct {
	Timeout         time.Duration   `mapstructure:"timeout"`
	MaxBytes        int64           `mapstructure:"max_bytes"`
	MaxContentChars int             `mapstructure:"max_content_chars"`
	MaxLinks        int             `mapstructure:"max_links"`
	CacheTTL        time.Duration   `mapstructure:"cache_ttl"` // 来源缓存与已完成分析的复用时限；0 禁用复用。
	BrowserFallback BrowserFallback `mapstructure:"browser_fallback"`
}

type BrowserFallback struct {
	Enabled bool `mapstructure:"enabled"`
	// Engine 是浏览器引擎；chromium 不代表必须使用系统 Chrome。
	Engine         string `mapstructure:"engine"`
	Mode           string `mapstructure:"mode"`
	ExecutablePath string `mapstructure:"executable_path"`
	Endpoint       string `mapstructure:"endpoint"`
	// Headless 只影响 launch；connect/cdp 连接远端浏览器时不生效。
	Headless bool `mapstructure:"headless"`
}

type Agent struct {
	MaxSources      int           `mapstructure:"max_sources"`      // 包括入口、原文、补读和失败尝试。
	MaxSupplemental int           `mapstructure:"max_supplemental"` // evidence/context 子预算。
	MaxDepth        int           `mapstructure:"max_depth"`        // 首个抓取页面为 0，追读每深入一次加 1。
	MaxIterations   int           `mapstructure:"max_iterations"`
	Timeout         time.Duration `mapstructure:"timeout"`
	Categories      []string      `mapstructure:"categories"`
}

type Weekly struct {
	Enabled  bool   `mapstructure:"enabled"`
	Timezone string `mapstructure:"timezone"`
	Weekday  int    `mapstructure:"weekday"`
	Time     string `mapstructure:"time"`
}

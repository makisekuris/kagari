package domain

import (
	"encoding/json"
	"time"
)

// Submission 保存一次收录的输入；重复转发仍是一条独立收录记录。
// UserID 是档案所有者，ChatID 是提交所在私聊，不一定等于结果投递的目标。
type Submission struct {
	UserID        int64     `json:"user_id"`
	ChatID        int64     `json:"chat_id"`
	Text          string    `json:"text"`
	ForwardedText string    `json:"forwarded_text,omitempty"`
	Note          string    `json:"note"`
	URLs          []string  `json:"urls"`
	ReceivedAt    time.Time `json:"received_at"` // 收录时间，决定周报范围；不同于文章发布日期。
	CacheKey      string    `json:"cache_key"`   // 相同用户、输入与分析偏好的复用键，不是任务幂等键。
}

type Link struct {
	URL     string `json:"url"`
	Text    string `json:"text"`
	Context string `json:"context"`
}

// Source 是实际取得的证据或读取失败记录。Content 保存提取正文，不保存原始 HTML。
// ok 表示读取完成；supplied 表示用户提供的文本，未经网页核验；
// incomplete 表示提取不完整，只有带正文且 Truncated 的片段才允许有限引用；
// restricted 表示访问或安全策略限制；failed 表示其他抓取失败。
type Source struct {
	ID           string     `json:"id"`
	RequestedURL string     `json:"requested_url"` // 原始入口，保留短链与重定向溯源。
	URL          string     `json:"url"`           // 最终来源 URL，用于稳定标识同一页面。
	Kind         string     `json:"kind"`
	Title        string     `json:"title"`
	Author       string     `json:"author"`
	PublishedAt  *time.Time `json:"published_at"`
	Content      string     `json:"content"`
	Links        []Link     `json:"links"`
	Status       string     `json:"status"`
	Reason       string     `json:"reason"`
	ReadMethod   string     `json:"read_method"`
	FetchedAt    time.Time  `json:"fetched_at"`
	Truncated    bool       `json:"truncated"`
}

// Reading 记录一次实际读取尝试；同一 URL 可通过不同后端重试。
type Reading struct {
	SourceID string `json:"source_id"`
	URL      string `json:"url"`
	Backend  string `json:"backend"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type ResultKind string

const (
	ResultKindAnalysis ResultKind = "analysis"
	ResultKindChat     ResultKind = "chat"
)

// Result 保存回复类型、正文及阅读记录。失败任务也可能包含部分结果，
// 由任务状态决定 Body 是否可发布。
// 历史记录缺少 Kind；仅在已完成分析的读取入口补为 analysis。
type Result struct {
	Kind            ResultKind `json:"kind,omitempty"`
	Body            string     `json:"body,omitempty"`
	AnalysisVersion string     `json:"analysis_version,omitempty"`
	Sources         []Source   `json:"sources"`
	Readings        []Reading  `json:"readings"`
	Usage           Usage      `json:"usage"`
	UsageReported   bool       `json:"usage_reported"`
	CreatedAt       time.Time  `json:"created_at"`
}

// Job 的处理状态与消息投递状态独立：completed 只表示产物和 outbox 已落库。
// Key 防止同一入口事件重复入队；Attempts 是已领取处理的次数。
type Job struct {
	ID            int64
	Kind          string
	Key           string
	Payload       json.RawMessage
	Result        json.RawMessage
	Targets       []DeliveryTarget // 入队时保存，处理和重试不读取当前分发配置。
	Status        string
	Attempts      int
	LastError     string
	NextAttemptAt time.Time `json:"next_attempt_at"`
}

// DeliveryTarget 标识渠道及该渠道的目标地址，不包含凭证。
type DeliveryTarget struct {
	Channel string `json:"channel"`
	Address string `json:"address"`
}

type ContentFormat string

const (
	ContentPlainText ContentFormat = ""
	ContentMarkdown  ContentFormat = "markdown"
)

// Content 描述正文语法；渠道适配器选择对应的发送方式。
type Content struct {
	Text   string
	Format ContentFormat
}

// Delivery 是产物的一段消息；Part 保证同一任务、同一目标按顺序投递。
// uncertain 表示渠道可能已经接收，需用户显式允许后才能重新发送。
type Delivery struct {
	ID       int64
	JobID    int64
	Target   DeliveryTarget
	Part     int
	Text     string
	Format   ContentFormat
	Status   string
	Attempts int
}

type ArchiveEntry struct {
	JobID      int64      `json:"job_id"`
	Submission Submission `json:"submission"`
	Result     Result     `json:"result"`
}

type Command struct {
	UserID int64  `json:"user_id"`
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

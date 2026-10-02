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

// Reading 记录一次阅读决策，多个入口可指向同一 Source。
// entry 是提交入口，primary 追读原文，evidence 核对事实，context 补充背景；
// Question 保留补读目的，Depth 从第一个抓取页面的 0 开始计算。
type Reading struct {
	SourceID string `json:"source_id"`
	ParentID string `json:"parent_id"`
	URL      string `json:"url"`
	Question string `json:"question"`
	Role     string `json:"role"`
	Depth    int    `json:"depth"`
}

// Claim 的 SourceIDs 必须指向本次已读取的可用来源；保存引用不代表已自动证明结论正确。
type Claim struct {
	Text      string   `json:"text"`
	SourceIDs []string `json:"source_ids"`
}

// AnalysisHeadings 是模型按人格和阅读偏好生成的展示文案，不改变对应内容的证据语义。
type AnalysisHeadings struct {
	Summary       string `json:"summary"`
	Discussion    string `json:"discussion"`
	Evaluation    string `json:"evaluation"`
	Uncertainties string `json:"uncertainties"`
	Sources       string `json:"sources"`
}

// Analysis 区分来源事实、讨论者观点和 Agent 判断，避免周报混淆三者。
type Analysis struct {
	Headings      *AnalysisHeadings `json:"headings,omitempty"` // nil 兼容旧归档。
	Title         string            `json:"title"`
	Overview      string            `json:"overview"`
	Summary       []Claim           `json:"summary"`
	Discussion    []Claim           `json:"discussion"`
	Evaluation    []Claim           `json:"evaluation"`
	Category      string            `json:"category"`
	Tags          []string          `json:"tags"`
	Relevance     string            `json:"relevance"`
	Uncertainties []string          `json:"uncertainties"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// Result 同时保存可发布的分析和其依据。分析失败时也可能包含 Sources/Readings，
// 由任务状态决定它是最终结果还是供诊断与重试使用的部分结果。
type Result struct {
	Analysis        Analysis  `json:"analysis"`
	AnalysisVersion string    `json:"analysis_version,omitempty"`
	Sources         []Source  `json:"sources"`
	Readings        []Reading `json:"readings"`
	Usage           Usage     `json:"usage"`
	CreatedAt       time.Time `json:"created_at"`
}

// Job 的处理状态与消息投递状态独立：completed 只表示产物和 outbox 已落库。
// Key 防止同一入口事件重复入队；Attempts 是已领取处理的次数。
type Job struct {
	ID           int64
	Kind         string
	Key          string
	Payload      json.RawMessage
	Result       json.RawMessage
	TargetChatID int64
	Status       string
	Attempts     int
	LastError    string
}

// Delivery 是产物的一段消息；Part 保证同一任务按顺序投递。
// uncertain 表示 Telegram 可能已经接收，需用户显式允许后才能重新发送。
type Delivery struct {
	ID       int64
	JobID    int64
	ChatID   int64
	Part     int
	Text     string
	Status   string
	Attempts int
}

type ArchiveEntry struct {
	JobID      int64      `json:"job_id"`
	Submission Submission `json:"submission"`
	Result     Result     `json:"result"`
}

// DigestRequest 按 UserID 和收录时间半开区间 [Start, End) 选择已完成分析。
type DigestRequest struct {
	UserID int64     `json:"user_id"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
}

type Command struct {
	UserID int64  `json:"user_id"`
	ChatID int64  `json:"chat_id"`
	Text   string `json:"text"`
}

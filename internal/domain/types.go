package domain

import "time"

// Submission 保存一个任务阶段的输入。
type Submission struct {
	Text          string   `json:"text"`
	ForwardedText string   `json:"forwarded_text,omitempty"`
	Note          string   `json:"note"`
	URLs          []string `json:"urls"`
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

// SourceRef identifies a persisted source without carrying its body.
type SourceRef struct {
	ID              string    `json:"id"`
	RunID           int64     `json:"run_id"`
	URL             string    `json:"url"`
	Title           string    `json:"title,omitempty"`
	Status          string    `json:"status"`
	FetchedAt       time.Time `json:"fetched_at"`
	SourceTruncated bool      `json:"source_truncated"`
	TotalRunes      int       `json:"total_runes"`
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

// Result 保存阶段摘要与读取记录；失败时仍可包含已取得的材料。
type Result struct {
	Body          string      `json:"body,omitempty"`
	Sources       []Source    `json:"sources"`
	SourceRefs    []SourceRef `json:"source_refs,omitempty"`
	Readings      []Reading   `json:"readings"`
	Usage         Usage       `json:"usage"`
	UsageReported bool        `json:"usage_reported"`
	CreatedAt     time.Time   `json:"created_at"`
}

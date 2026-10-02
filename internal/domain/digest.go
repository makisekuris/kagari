package domain

import "time"

const DigestVersion = "weekly-review-v1"

// DigestRequest 按用户和收录时间半开区间 [Start, End) 选择已完成分析。
// End 是调用方捕获的截止时间；空 Version 兼容升级前已入队的任务。
type DigestRequest struct {
	UserID  int64     `json:"user_id"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Version string    `json:"version,omitempty"`
}

// DigestInput 是首次处理时冻结的模型输入，不包含密钥、聊天路由和来源全文。
// AsOf 是归档读取时刻，Cutoff 是材料范围上界；重试复用人格指令和偏好。
type DigestInput struct {
	UserID      int64         `json:"user_id"`
	Start       time.Time     `json:"start"`
	Cutoff      time.Time     `json:"cutoff"`
	AsOf        time.Time     `json:"as_of"`
	Timezone    string        `json:"timezone"`
	Profile     string        `json:"profile"`
	Instruction string        `json:"instruction,omitempty"`
	Entries     []DigestEntry `json:"entries"`
}

type DigestEntry struct {
	JobID      int64          `json:"job_id"`
	ReceivedAt time.Time      `json:"received_at"`
	Analysis   Analysis       `json:"analysis"`
	Sources    []DigestSource `json:"sources"`
}

// Usable 由程序依据实际读取状态和正文计算，模型不能改变证据可用性。
type DigestSource struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	RequestedURL string `json:"requested_url"`
	URL          string `json:"url"`
	Status       string `json:"status"`
	Reason       string `json:"reason"`
	Truncated    bool   `json:"truncated"`
	Usable       bool   `json:"usable"`
}

type DigestReview struct {
	Opening  string          `json:"opening"`
	Sections []DigestSection `json:"sections"`
	Closing  string          `json:"closing"`
}

type DigestSection struct {
	Name  string       `json:"name"`
	Items []DigestItem `json:"items"`
}

// EntryIDs 构成输入条目的完整分区：每个输入恰好属于一个回顾条目。
type DigestItem struct {
	EntryIDs []int64     `json:"entry_ids"`
	Title    string      `json:"title"`
	Review   string      `json:"review"`
	Refs     []DigestRef `json:"refs"`
}

// 引用使用归档任务和来源的二元组，不能仅按来源 ID 跨任务匹配。
type DigestRef struct {
	JobID    int64  `json:"job_id"`
	SourceID string `json:"source_id"`
}

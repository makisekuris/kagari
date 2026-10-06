package domain

import "time"

const DigestVersion = "weekly-text-v1"

// DigestRequest 按用户和收录时间半开区间 [Start, End) 选择已完成分析。
// End 是调用方捕获的截止时间。Version 必须匹配当前契约。
type DigestRequest struct {
	UserID  int64     `json:"user_id"`
	Start   time.Time `json:"start"`
	End     time.Time `json:"end"`
	Version string    `json:"version"`
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
	Instruction string        `json:"instruction"`
	Entries     []DigestEntry `json:"entries"`
}

type DigestEntry struct {
	JobID      int64          `json:"job_id"`
	ReceivedAt time.Time      `json:"received_at"`
	Body       string         `json:"body"`
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

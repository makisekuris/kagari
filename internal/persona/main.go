package persona

import taffy "kagari/internal/persona/taffy"

// PersonaRole 是人格包向调用方暴露的能力，不暴露具体角色类型。
type PersonaRole interface {
	Prompt() string
	AskChatID(jobID int64) string
}

// Default 集中选择当前使用的人格，替换实现无需修改调用方。
func Default() PersonaRole {
	return taffy.Taffy{}
}

package assistant

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/textclip"
)

type setUserQuestionsArgs struct {
	Questions *[]string `json:"questions,omitempty" jsonschema:"description=Omit or set null to leave pending questions unchanged; an empty array clears them and a nonempty array replaces them."`
}

func (p *personalTools) replyTools() ([]tool.BaseTool, error) {
	questionsTool, err := utils.InferTool("set_user_questions", "仅当当前用户请求缺少必要信息、目标选择或授权时，设置本轮必须提出的问题完整列表。请求已完成或不需要用户回答时不要添加可选追问。省略 questions 或设为 null 表示不变；[] 撤回尚未发送的问题；非空数组完整替换。每个问题须具体且非空，仅直接用户事件可调用。已有未解决任务操作失败会独立强制通知，不受问题列表清空影响。不要用“无”或“无需补充”作占位，也不要为普通链接阅读索要确认；任务回调使用 settle_task 的 waiting_input。", func(_ context.Context, args setUserQuestionsArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
		if args.Questions == nil {
			return personalToolReply{OK: true}, nil
		}
		questions := make([]string, 0, len(*args.Questions))
		seen := make(map[string]bool, len(*args.Questions))
		for _, raw := range *args.Questions {
			question := strings.TrimSpace(raw)
			if question == "" || utf8.RuneCountInString(question) > personal.MaxTaskChars {
				return toolError(fmt.Errorf("each question must be nonblank and at most %d characters", personal.MaxTaskChars)), nil
			}
			if !seen[question] {
				seen[question] = true
				questions = append(questions, question)
			}
		}
		p.questions = questions
		return personalToolReply{OK: true}, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{questionsTool}, nil
}

func (p *personalTools) requiredTaskReply() string {
	var parts []string
	for _, failure := range p.unresolvedTaskActions() {
		operation := "创建任务"
		switch failure.Operation {
		case "update_task":
			operation = "更新任务"
		case "continue_task":
			operation = "继续执行"
		case "add_task_input":
			operation = "补充材料"
		}
		parts = append(parts, fmt.Sprintf("「%s」未能%s。这部分尚未完成，可以让我重试。", textclip.Excerpt(failure.Target, 80), operation))
	}
	parts = append(parts, p.questions...)
	return strings.Join(parts, "\n\n")
}

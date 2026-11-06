package personal

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

type askUserArgs struct {
	Question string `json:"question"`
}

func (p *personalTools) replyTools() ([]tool.BaseTool, error) {
	ask, err := utils.InferTool("ask_user", "当前用户请求仍缺少必要信息、目标选择或授权时，指定本轮必须向用户提出的问题。已有后台工作不会覆盖该问题。不要为普通链接阅读索要确认；任务回调应使用 settle_task 的 waiting_input。", func(_ context.Context, args askUserArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return toolError(err), nil
		}
		question := strings.TrimSpace(args.Question)
		if question == "" || runeLen(question) > maxTaskChars {
			return toolError(fmt.Errorf("question must be nonblank and at most %d characters", maxTaskChars)), nil
		}
		for _, existing := range p.questions {
			if existing == question {
				return personalToolReply{OK: true}, nil
			}
		}
		p.questions = append(p.questions, question)
		return personalToolReply{OK: true}, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{ask}, nil
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
		parts = append(parts, fmt.Sprintf("「%s」未能%s。这部分尚未完成，可以让我重试。", clip(failure.Target, 80), operation))
	}
	parts = append(parts, p.questions...)
	return strings.Join(parts, "\n\n")
}

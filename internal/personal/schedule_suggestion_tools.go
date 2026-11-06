package personal

import (
	"context"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

type suggestionToolReply struct {
	OK         bool                          `json:"ok"`
	Error      string                        `json:"error,omitempty"`
	Replayed   bool                          `json:"replayed,omitempty"`
	Suggestion *ScheduleSuggestion           `json:"suggestion,omitempty"`
	Resolution *ScheduleSuggestionResolution `json:"resolution,omitempty"`
}

func (p *personalTools) buildScheduleSuggestionTools() ([]tool.BaseTool, error) {
	propose, err := utils.InferTool("propose_schedule_context_update", "仅在当前计划的 Task 回调中记录背景差异。memory_refs 的 key/version 来自当前记忆，evidence 必须逐字摘录该记忆的 value，不是回调事件原文。replacements.old 必须逐字摘录计划背景且唯一出现；new 是建议替换片段。不得更改目标或时间。建议由程序随本次通知附上，不要自行复述已提醒的建议。", func(ctx context.Context, a struct {
		TaskID   int64                   `json:"task_id"`
		Proposal ScheduleContextProposal `json:"proposal"`
	}) (suggestionToolReply, error) {
		if err := p.ownTask(a.TaskID); err != nil {
			return suggestionToolReply{Error: err.Error()}, nil
		}
		key, err := p.requestKey("propose_schedule_context_update", a)
		if err != nil {
			return suggestionToolReply{Error: err.Error()}, nil
		}
		s, replayed, err := p.store.ProposeScheduleContextUpdate(ctx, p.event.ID, key, a.TaskID, a.Proposal, time.Now().UTC())
		if err != nil {
			return suggestionToolReply{Error: err.Error()}, nil
		}
		p.addScheduleID(s.ScheduleID)
		return suggestionToolReply{OK: true, Replayed: replayed, Suggestion: &s}, nil
	})
	if err != nil {
		return nil, err
	}
	resolve, err := utils.InferTool("resolve_schedule_context_update", "根据用户选择处理建议：用户确认同步时用 apply，即使已发现记忆变化也由 Store 校验并返回新候选；仅用户明确要求保留原设置时用 keep，不用 keep 代替过期校验。需建议编号和已展示版本。过期返回新差异，须再次确认；不能在本回合自动重试 apply。", func(ctx context.Context, a struct {
		SuggestionID     int64  `json:"suggestion_id"`
		ExpectedRevision int64  `json:"expected_revision"`
		Decision         string `json:"decision"`
	}) (suggestionToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return suggestionToolReply{Error: err.Error()}, nil
		}
		p.mustNotify = true
		if a.SuggestionID <= 0 || a.ExpectedRevision <= 0 {
			return suggestionToolReply{Error: "suggestion_id and expected_revision must be positive"}, nil
		}
		key, err := p.requestKey("resolve_schedule_context_update", a)
		if err != nil {
			return suggestionToolReply{Error: err.Error()}, nil
		}
		r, replayed, err := p.store.ResolveScheduleContextUpdate(ctx, p.event.ID, key, a.SuggestionID, a.ExpectedRevision, a.Decision, time.Now().UTC())
		if err != nil {
			return suggestionToolReply{Error: err.Error()}, nil
		}
		p.addScheduleID(r.Suggestion.ScheduleID)
		return suggestionToolReply{OK: true, Replayed: replayed, Resolution: &r}, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{propose, resolve}, nil
}

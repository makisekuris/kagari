package assistant

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/personal"
)

const memoryCurrentAnswerToolName = "memory_answer_current"

// Memory tools expose bounded reads and explicit user-authorized writes.
func (p *personalTools) memoryTools() ([]tool.BaseTool, error) {
	answerCurrent, err := utils.InferTool(memoryCurrentAnswerToolName, "仅用于用户单独询问当前长期记忆值。用相关关键词检索；如果已知精确键，同时传 key。返回的 current_match=true 才表示找到当前有效记忆；false 只表示没有匹配到，不证明现实中不存在该事实。更大任务的上下文读取用 memory_search/memory_get，显式历史审计用 memory_history。", func(ctx context.Context, a memoryCurrentAnswerArgs) (memoryCurrentAnswerReply, error) {
		if p.event.Kind != personal.EventUser {
			return memoryCurrentAnswerReply{OK: false, Error: "current memory answers require a direct user event"}, nil
		}
		if strings.TrimSpace(a.Key) == "" && strings.TrimSpace(a.Query) == "" {
			return memoryCurrentAnswerReply{OK: false, Error: "key or query is required"}, nil
		}

		matches := make([]personal.Memory, 0, 1)
		tombstone := false
		keyFound := false
		if key := strings.TrimSpace(a.Key); key != "" {
			memory, err := p.store.Memory(ctx, key)
			if err != nil {
				return memoryCurrentAnswerReply{OK: false, Error: err.Error()}, nil
			}
			if memory != nil {
				keyFound = true
				if memory.Deleted {
					tombstone = true
				} else {
					matches = append(matches, *memory)
				}
			}
		}
		if !keyFound {
			query := strings.TrimSpace(a.Query)
			if query != "" {
				memories, err := p.store.Memories(ctx, query, 20)
				if err != nil {
					return memoryCurrentAnswerReply{OK: false, Error: err.Error()}, nil
				}
				for _, memory := range memories {
					if memory.Deleted {
						tombstone = true
						continue
					}
					matches = append(matches, memory)
				}
			}
		}

		p.recordCurrentMemoryAnswer(len(matches) > 0)
		result := memoryCurrentAnswerReply{OK: true, CurrentMatch: len(matches) > 0, Tombstone: tombstone}
		if len(matches) == 1 {
			result.Memory = &matches[0]
		} else if len(matches) > 1 {
			result.Memories = matches
		}
		return result, nil
	})
	if err != nil {
		return nil, err
	}

	memorySearch, err := utils.InferTool("memory_search", "搜索当前生效的长期事实和偏好，主要用于更大任务或混合请求的上下文读取；不返回已遗忘的值。用户单独询问当前记忆值时使用 memory_answer_current。", func(ctx context.Context, a memorySearchArgs) (personalToolReply, error) {
		if strings.TrimSpace(a.Query) == "" {
			return toolError(fmt.Errorf("query is required")), nil
		}
		memories, err := p.store.Memories(ctx, a.Query, 20)
		if err != nil {
			return toolError(err), nil
		}
		active := memories[:0]
		for _, memory := range memories {
			if !memory.Deleted {
				active = append(active, memory)
			}
		}
		p.recordSupplementalWork(personal.DeliveryControl)
		return personalToolReply{OK: true, Memories: active}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryGet, err := utils.InferTool("memory_get", "按已知的精确键读取当前记忆，主要用于更大任务的上下文。已遗忘的键只返回 tombstone 版本，不含旧值；这仅表示该记忆已删除，不表示对象没有名称或事实。用户单独询问当前记忆值时使用 memory_answer_current。", func(ctx context.Context, a memoryKeyArgs) (personalToolReply, error) {
		if strings.TrimSpace(a.Key) == "" {
			return toolError(fmt.Errorf("key is required")), nil
		}
		memory, err := p.store.Memory(ctx, a.Key)
		if err != nil {
			return toolError(err), nil
		}
		if memory == nil {
			return toolError(fmt.Errorf("memory not found")), nil
		}
		if memory.Deleted {
			copy := *memory
			copy.Value = ""
			memory = &copy
		}
		p.recordSupplementalWork(personal.DeliveryControl)
		return personalToolReply{OK: true, Memory: memory}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryPut, err := utils.InferTool("memory_put", "仅按用户当前消息中的明确记忆请求保存或更正，成功后立即持久化。执行某次操作所需的设置或提醒不属于记忆请求。evidence 必须是当前消息的原文子串。", func(ctx context.Context, a memoryPutArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return p.memoryError(a.Key, "put", err), nil
		}
		p.mustNotify = true
		if strings.TrimSpace(a.Key) == "" || strings.TrimSpace(a.Value) == "" || a.ExpectedVersion < 0 || !strings.Contains(p.event.Text, a.Evidence) || strings.TrimSpace(a.Evidence) == "" {
			return p.memoryError(a.Key, "put", fmt.Errorf("key, value, nonnegative expected_version and an exact current-message evidence quote are required")), nil
		}
		key, err := p.requestKey("memory_put", a)
		if err != nil {
			return p.memoryError(a.Key, "put", err), nil
		}
		memory, err := p.store.WriteMemory(ctx, p.event.ID, key, personal.MemoryChange{Operation: "put", Key: a.Key, Value: a.Value, ExpectedVersion: a.ExpectedVersion, Evidence: a.Evidence})
		if err != nil {
			return p.memoryError(a.Key, "put", err), nil
		}
		p.recordSupplementalWork(personal.DeliveryControl)
		return personalToolReply{OK: true, Memory: &memory}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryForget, err := utils.InferTool("memory_forget", "仅按用户当前消息中的明确遗忘请求删除。提供当前版本；evidence 必须是当前消息原文。成功后立即持久化。向用户确认时不复述或转述被删除的值。", func(ctx context.Context, a memoryForgetArgs) (personalToolReply, error) {
		if err := p.directUserMutation(); err != nil {
			return p.memoryError(a.Key, "forget", err), nil
		}
		p.mustNotify = true
		if strings.TrimSpace(a.Key) == "" || a.ExpectedVersion <= 0 || !strings.Contains(p.event.Text, a.Evidence) || strings.TrimSpace(a.Evidence) == "" {
			return p.memoryError(a.Key, "forget", fmt.Errorf("key, positive expected_version and an exact current-message evidence quote are required")), nil
		}
		key, err := p.requestKey("memory_forget", a)
		if err != nil {
			return p.memoryError(a.Key, "forget", err), nil
		}
		memory, err := p.store.WriteMemory(ctx, p.event.ID, key, personal.MemoryChange{Operation: "forget", Key: a.Key, ExpectedVersion: a.ExpectedVersion, Evidence: a.Evidence})
		if err != nil {
			return p.memoryError(a.Key, "forget", err), nil
		}
		p.recordSupplementalWork(personal.DeliveryControl)
		return personalToolReply{OK: true, Memory: &memory}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryHistory, err := utils.InferTool("memory_history", "仅在用户明确要求审计或查看历史记忆变更时使用；不把历史值恢复为当前记忆。", func(ctx context.Context, a memoryHistoryArgs) (personalToolReply, error) {
		history, err := p.store.MemoryHistory(ctx, strings.TrimSpace(a.Key), 50)
		if err != nil {
			return toolError(err), nil
		}
		p.recordSupplementalWork(personal.DeliveryControl)
		return personalToolReply{OK: true, Memories: history}, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{answerCurrent, memorySearch, memoryGet, memoryPut, memoryForget, memoryHistory}, nil
}

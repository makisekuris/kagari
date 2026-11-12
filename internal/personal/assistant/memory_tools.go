package assistant

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/personal"
)

// Memory tools expose bounded reads and explicit user-authorized writes.
func (p *personalTools) memoryTools() ([]tool.BaseTool, error) {
	memorySearch, err := utils.InferTool("memory_search", "搜索当前生效的长期事实和偏好，不返回已遗忘的值。", func(ctx context.Context, a memorySearchArgs) (personalToolReply, error) {
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
		return personalToolReply{OK: true, Memories: active}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryGet, err := utils.InferTool("memory_get", "按键读取记忆。已遗忘的键返回 tombstone 版本，不含旧值。", func(ctx context.Context, a memoryKeyArgs) (personalToolReply, error) {
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
		return personalToolReply{OK: true, Memory: memory}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryPut, err := utils.InferTool("memory_put", "仅按用户当前消息中的明确记忆请求保存或更正，成功后立即持久化。evidence 必须是当前消息的原文子串。", func(ctx context.Context, a memoryPutArgs) (personalToolReply, error) {
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
		return personalToolReply{OK: true, Memory: &memory}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryForget, err := utils.InferTool("memory_forget", "仅按用户当前消息中的明确遗忘请求删除。提供当前版本；evidence 必须是当前消息原文。成功后立即持久化。", func(ctx context.Context, a memoryForgetArgs) (personalToolReply, error) {
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
		return personalToolReply{OK: true, Memory: &memory}, nil
	})
	if err != nil {
		return nil, err
	}

	memoryHistory, err := utils.InferTool("memory_history", "查看当前键的历史记忆变更；不使用已删除旧值恢复主动记忆。", func(ctx context.Context, a memoryHistoryArgs) (personalToolReply, error) {
		history, err := p.store.MemoryHistory(ctx, strings.TrimSpace(a.Key), 50)
		if err != nil {
			return toolError(err), nil
		}
		return personalToolReply{OK: true, Memories: history}, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{memorySearch, memoryGet, memoryPut, memoryForget, memoryHistory}, nil
}

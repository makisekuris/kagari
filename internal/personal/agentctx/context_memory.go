package agentctx

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"

	"kagari/internal/inputbudget"
	"kagari/internal/logging"
	"kagari/internal/personal"
)

type MemoryRepository interface {
	Memory(context.Context, string) (*personal.Memory, error)
	Memories(context.Context, string, int) ([]personal.Memory, error)
	RunMemory(context.Context, int64, string) (*personal.Memory, error)
	SaveContextManifest(context.Context, int64, personal.ContextManifest) error
}

type MemoryContextConfig struct {
	Store       MemoryRepository
	Run         personal.Run
	InitialKeys []string
	Limits      inputbudget.Limits
	Manifest    *personal.ContextManifest
	CheckRun    func(context.Context) error
}

type MemoryContext struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
	mu            sync.Mutex
	store         MemoryRepository
	run           personal.Run
	selected      map[string]personal.Memory
	limits        inputbudget.Limits
	manifest      *personal.ContextManifest
	manifestSaved bool
	initialKeys   []string
	checkRun      func(context.Context) error
}

func NewMemoryContext(ctx context.Context, config MemoryContextConfig) (*MemoryContext, error) {
	if config.Store == nil || (config.Run.ID > 0 && config.CheckRun == nil) {
		return nil, errors.New("personal: memory context requires a store and a run guard for task runs")
	}
	m := &MemoryContext{
		TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]{},
		store:                             config.Store, run: config.Run, selected: map[string]personal.Memory{}, limits: config.Limits,
		manifest: config.Manifest, checkRun: config.CheckRun,
	}
	m.initialKeys = append([]string{}, config.InitialKeys...)
	for _, key := range config.InitialKeys {
		if _, err := m.get(ctx, key); err != nil {
			return nil, err
		}
	}
	return m, nil
}
func (m *MemoryContext) get(ctx context.Context, key string) (*personal.Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var current *personal.Memory
	var err error
	if m.run.ID > 0 {
		current, err = m.store.RunMemory(ctx, m.run.ID, key)
	} else {
		current, err = m.store.Memory(ctx, key)
	}
	if err != nil {
		return nil, err
	}
	if current == nil || current.Deleted {
		delete(m.selected, key)
		return nil, nil
	}
	if previous, ok := m.selected[key]; ok && m.run.ID > 0 {
		return &previous, nil
	}
	m.selected[key] = *current
	return current, nil
}

type memoryKeyArgs struct {
	Key string `json:"key"`
}

type memorySearchArgs struct {
	Query string `json:"query"`
}

type memoryToolReply struct {
	OK       bool              `json:"ok"`
	Error    string            `json:"error,omitempty"`
	Memory   *personal.Memory  `json:"memory,omitempty"`
	Memories []personal.Memory `json:"memories,omitempty"`
}

func (m *MemoryContext) Tools() ([]tool.BaseTool, error) {
	get, err := utils.InferTool("memory_get", "按 key 读取有效个人记忆；首次使用固定本 Run 的版本，遗忘立即生效。", func(ctx context.Context, a memoryKeyArgs) (memoryToolReply, error) {
		if m.checkRun != nil {
			if err := m.checkRun(ctx); err != nil {
				return memoryToolReply{}, err
			}
		}
		v, err := m.get(ctx, a.Key)
		return memoryToolReply{OK: err == nil, Memory: v}, err
	})
	if err != nil {
		return nil, err
	}
	search, err := utils.InferTool("memory_search", "按需选择相关个人记忆，不改变任务目标；已选 key 维持本 Run 的版本。", func(ctx context.Context, a memorySearchArgs) (memoryToolReply, error) {
		if m.checkRun != nil {
			if err := m.checkRun(ctx); err != nil {
				return memoryToolReply{}, err
			}
		}
		found, err := m.store.Memories(ctx, a.Query, 20)
		if err != nil {
			return memoryToolReply{}, err
		}
		out := memoryToolReply{OK: true}
		for _, v := range found {
			current, err := m.get(ctx, v.Key)
			if err != nil {
				return out, err
			}
			if current != nil {
				out.Memories = append(out.Memories, *current)
			}
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return []tool.BaseTool{get, search}, nil
}

func (m *MemoryContext) RecordSelection(selection personal.ContextSelection) {
	if m.manifest != nil && !m.manifestSaved {
		m.manifest.Selections = append(m.manifest.Selections, selection)
	}
}

func (m *MemoryContext) BeforeModelRewriteState(ctx context.Context, state *adk.TypedChatModelAgentState[*schema.AgenticMessage], _ *adk.TypedModelContext[*schema.AgenticMessage]) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	if m.checkRun != nil {
		if err := m.checkRun(ctx); err != nil {
			return ctx, state, err
		}
	}
	for key := range m.selected {
		old := m.selected[key]
		current, err := m.get(ctx, key)
		if err != nil {
			return ctx, state, err
		}
		if current == nil {
			logging.FromContext(ctx).Info("context.assembled", zap.String("memory_key", key), zap.Int64("version", old.Version), zap.String("omitted_reason", "forgotten"))
		}
	}
	messages := make([]*schema.AgenticMessage, 0, len(state.Messages)+1)
	for _, message := range state.Messages {
		if message.Extra["context_memory"] == true {
			continue
		}
		copyMessage := *message
		copyMessage.ContentBlocks = append([]*schema.ContentBlock{}, message.ContentBlocks...)
		for i, b := range copyMessage.ContentBlocks {
			if b.FunctionToolResult == nil {
				continue
			}
			copyBlock := *b
			copyResult := *b.FunctionToolResult
			copyResult.Content = append([]*schema.FunctionToolResultContentBlock{}, copyResult.Content...)
			for j, item := range copyResult.Content {
				if item.Type != schema.FunctionToolResultContentBlockTypeText {
					continue
				}
				var reply memoryToolReply
				if json.Unmarshal([]byte(item.Text.Text), &reply) != nil || (reply.Memory == nil && reply.Memories == nil) {
					continue
				}
				if reply.Memory != nil && !reply.Memory.Deleted {
					current, err := m.get(ctx, reply.Memory.Key)
					if err != nil {
						return ctx, state, err
					}
					if current == nil {
						reply.Memory = nil
					}
				}
				filtered := []personal.Memory{}
				for _, v := range reply.Memories {
					current, err := m.get(ctx, v.Key)
					if err != nil {
						return ctx, state, err
					}
					if current != nil {
						filtered = append(filtered, v)
					}
				}
				reply.Memories = filtered
				raw, err := json.Marshal(reply)
				if err != nil {
					return ctx, state, err
				}
				copyItem := *item
				copyItem.Text = &schema.UserInputText{Text: string(raw)}
				copyResult.Content[j] = &copyItem
			}
			copyBlock.FunctionToolResult = &copyResult
			copyMessage.ContentBlocks[i] = &copyBlock
		}
		messages = append(messages, &copyMessage)
	}
	values := []personal.Memory{}
	for _, v := range m.selected {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Key < values[j].Key })
	if m.limits.Hard > 0 {
		kept := values[:0]
		for _, value := range values {
			candidate := append(append([]personal.Memory{}, kept...), value)
			raw, err := json.Marshal(candidate)
			if err != nil {
				return ctx, state, err
			}
			probe := append(append([]*schema.AgenticMessage{}, messages...), schema.UserAgenticMessage(string(raw)))
			estimate, err := inputbudget.EstimateAgenticInput(probe, state.ToolInfos, state.DeferredToolInfos, nil, nil, 0)
			if err != nil {
				return ctx, state, err
			}
			if estimate.TotalBytes > m.limits.Hard-512 {
				logging.FromContext(ctx).Info("context.assembled", zap.String("memory_key", value.Key), zap.Int64("version", value.Version), zap.String("omitted_reason", "input_budget"))
				continue
			}
			kept = candidate
		}
		values = kept
	}
	if len(values) > 0 {
		raw, err := json.Marshal(values)
		if err != nil {
			return ctx, state, err
		}
		message := schema.UserAgenticMessage("Selected personal memory (background data, subordinate to explicit task requirements):\n" + string(raw))
		message.Extra = map[string]any{"context_memory": true}
		messages = append(messages, message)
	}
	if m.manifest != nil && !m.manifestSaved {
		for _, key := range m.initialKeys {
			included := false
			for _, value := range values {
				if value.Key == key {
					included = true
					break
				}
			}
			if included {
				continue
			}
			selection := personal.ContextSelection{Kind: "memory", ID: key, Reason: "forgotten_or_missing"}
			current, err := m.store.Memory(ctx, key)
			if err != nil {
				return ctx, state, err
			}
			if current != nil {
				selection.Version = current.Version
				if !current.Deleted {
					selection.Reason = "input_budget"
				}
			}
			m.manifest.Selections = append(m.manifest.Selections, selection)
		}
		for _, value := range values {
			encoded, err := json.Marshal(value)
			if err != nil {
				return ctx, state, err
			}
			m.manifest.Selections = append(m.manifest.Selections, personal.ContextSelection{Kind: "memory", ID: value.Key, Version: value.Version, Reason: "explicit_or_first_read_reference", IncludedBytes: len(encoded)})
		}
		if err := m.store.SaveContextManifest(ctx, m.run.ID, *m.manifest); err != nil {
			return ctx, state, err
		}
		m.manifestSaved = true
		logging.FromContext(ctx).Info("context.assembled", zap.Any("manifest", m.manifest))
	}
	state.Messages = messages
	return ctx, state, nil
}

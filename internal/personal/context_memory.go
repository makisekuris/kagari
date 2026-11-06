package personal

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
	"kagari/internal/inputbudget"
	"kagari/internal/logging"
)

type ContextSelection struct {
	Kind           string `json:"kind"`
	ID             string `json:"id"`
	Version        int64  `json:"version,omitempty"`
	Reason         string `json:"reason"`
	IncludedBytes  int    `json:"included_bytes"`
	ContentVersion string `json:"content_version,omitempty"`
}
type ContextManifest struct {
	FormatVersion   int                `json:"format_version"`
	Revision        int64              `json:"revision"`
	AssembledAt     time.Time          `json:"assembled_at"`
	Selections      []ContextSelection `json:"selections"`
	ContextWindow   int64              `json:"context_window"`
	MaxOutputTokens int64              `json:"max_output_tokens"`
}
type ContextManifestRepository interface {
	SaveContextManifest(context.Context, int64, ContextManifest) error
	ReadCheckpoints(context.Context, int64) ([]ReadCheckpoint, error)
	RunMemory(context.Context, int64, string) (*Memory, error)
}

func (s *Store) ReadCheckpoints(ctx context.Context, runID int64) ([]ReadCheckpoint, error) {
	r, err := activeRunTx(ctx, s.db, runID)
	if err != nil {
		return nil, err
	}
	return s.readingCheckpoints(ctx, *r)
}

func (s *Store) SaveContextManifest(ctx context.Context, runID int64, m ContextManifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := activeRunTx(ctx, tx, runID)
	if err != nil {
		return err
	}
	if r.Revision != m.Revision || m.FormatVersion != 1 {
		return errors.New("personal: invalid context manifest")
	}
	_, err = tx.ExecContext(ctx, `UPDATE personal_runs SET context_manifest=? WHERE id=?`, string(raw), runID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type runMemoryContext struct {
	*adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]
	mu            sync.Mutex
	store         Repository
	run           Run
	selected      map[string]Memory
	limits        inputbudget.Limits
	manifest      *ContextManifest
	manifestSaved bool
	initialKeys   []string
}

func newRunMemoryContext(ctx context.Context, store Repository, run Run, keys []string) (*runMemoryContext, error) {
	m := &runMemoryContext{TypedBaseChatModelAgentMiddleware: &adk.TypedBaseChatModelAgentMiddleware[*schema.AgenticMessage]{}, store: store, run: run, selected: map[string]Memory{}}
	m.initialKeys = append([]string{}, keys...)
	for _, key := range keys {
		if _, err := m.get(ctx, key); err != nil {
			return nil, err
		}
	}
	return m, nil
}
func (m *runMemoryContext) get(ctx context.Context, key string) (*Memory, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var current *Memory
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
func (m *runMemoryContext) tools() ([]tool.BaseTool, error) {
	get, err := utils.InferTool("memory_get", "按 key 读取有效个人记忆；首次使用固定本 Run 的版本，遗忘立即生效。", func(ctx context.Context, a memoryKeyArgs) (personalToolReply, error) {
		if err := checkTaskRun(ctx, m.store, m.run); err != nil {
			return personalToolReply{}, err
		}
		v, err := m.get(ctx, a.Key)
		return personalToolReply{OK: err == nil, Memory: v}, err
	})
	if err != nil {
		return nil, err
	}
	search, err := utils.InferTool("memory_search", "按需选择相关个人记忆，不改变任务目标；已选 key 维持本 Run 的版本。", func(ctx context.Context, a memorySearchArgs) (personalToolReply, error) {
		if err := checkTaskRun(ctx, m.store, m.run); err != nil {
			return personalToolReply{}, err
		}
		found, err := m.store.Memories(ctx, a.Query, 20)
		if err != nil {
			return personalToolReply{}, err
		}
		out := personalToolReply{OK: true}
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
func (m *runMemoryContext) BeforeModelRewriteState(ctx context.Context, state *adk.TypedChatModelAgentState[*schema.AgenticMessage], _ *adk.TypedModelContext[*schema.AgenticMessage]) (context.Context, *adk.TypedChatModelAgentState[*schema.AgenticMessage], error) {
	if m.run.ID > 0 {
		if err := checkTaskRun(ctx, m.store, m.run); err != nil {
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
				var reply personalToolReply
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
				filtered := []Memory{}
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
	values := []Memory{}
	for _, v := range m.selected {
		values = append(values, v)
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Key < values[j].Key })
	if m.limits.Hard > 0 {
		kept := values[:0]
		for _, value := range values {
			candidate := append(append([]Memory{}, kept...), value)
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
			selection := ContextSelection{Kind: "memory", ID: key, Reason: "forgotten_or_missing"}
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
			m.manifest.Selections = append(m.manifest.Selections, ContextSelection{Kind: "memory", ID: value.Key, Version: value.Version, Reason: "explicit_or_first_read_reference", IncludedBytes: len(encoded)})
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

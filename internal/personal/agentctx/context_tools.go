package agentctx

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/channel"
	"kagari/internal/personal"
	"kagari/internal/textpage"
)

type ReadLedger struct {
	mu        sync.Mutex
	intervals map[string][][2]int
}

func NewReadLedger() *ReadLedger              { return &ReadLedger{intervals: map[string][][2]int{}} }
func readKey(kind, id, version string) string { return kind + "\x00" + id + "\x00" + version }
func (l *ReadLedger) Observe(kind, id string, p textpage.Page) {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := readKey(kind, id, p.Version)
	l.intervals[key] = append(l.intervals[key], [2]int{p.Offset, p.End})
}
func (l *ReadLedger) covered(u personal.ReadCheckpointUpdate) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	ranges := append([][2]int{}, l.intervals[readKey(u.ResourceKind, u.ResourceID, u.Version)]...)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i][0] < ranges[j][0] })
	end := u.ExpectedOffset
	for _, r := range ranges {
		if r[0] > end {
			break
		}
		end = max(end, r[1])
	}
	return end >= u.NextOffset
}

type ReadRepository interface {
	channel.Files
	ReadMaterial(context.Context, personal.ReadScope, string, string, textpage.Request) (personal.MaterialText, error)
	ListMaterials(context.Context, personal.ReadScope, int64, personal.ListPage) (personal.MaterialPage, error)
	SearchMaterial(context.Context, personal.ReadScope, string, string, personal.SearchPage) (personal.MatchPage, error)
	ReadRecord(context.Context, personal.ReadScope, personal.RecordRequest) (personal.RecordText, error)
	ReadCheckpoints(context.Context, int64) ([]personal.ReadCheckpoint, error)
	SaveReadCheckpoint(context.Context, int64, personal.ReadCheckpointUpdate) (personal.ReadCheckpoint, error)
	QueryRuns(context.Context, personal.ReadScope, int64, personal.ListPage) (personal.RunPage, error)
	Run(context.Context, int64) (*personal.Run, error)
}

type ReadToolsConfig struct {
	Store       ReadRepository
	Scope       personal.ReadScope
	Ledger      *ReadLedger
	ReadFile    func(context.Context, string, textpage.Request) (textpage.Page, error)
	Attachments map[string]bool
}

func ReadTools(config ReadToolsConfig) ([]tool.BaseTool, error) {
	if config.Store == nil || (config.Scope.RunID > 0 && config.Ledger == nil) {
		return nil, errors.New("personal: read tools require a store and task read ledger")
	}
	c := &readTools{
		store: config.Store, scope: config.Scope, ledger: config.Ledger,
		readFile: config.ReadFile, attachments: config.Attachments,
	}
	return c.build()
}

type readTools struct {
	store       ReadRepository
	scope       personal.ReadScope
	ledger      *ReadLedger
	readFile    func(context.Context, string, textpage.Request) (textpage.Page, error)
	attachments map[string]bool
}

func (c *readTools) build() ([]tool.BaseTool, error) {
	readMaterial, err := utils.InferTool("read_material", "读取已保存材料的一页；offset/end 为码点位置，分页不联网。", func(ctx context.Context, a struct {
		MaterialID string `json:"material_id"`
		Part       string `json:"part,omitempty"`
		textpage.Request
	}) (personal.MaterialText, error) {
		out, err := c.store.ReadMaterial(ctx, c.scope, a.MaterialID, a.Part, a.Request)
		if err != nil {
			return out, err
		}
		_, err = textpage.EncodeBounded(&out, textpage.MaxBytes(ctx), out.Page.Shrink)
		if err == nil && c.ledger != nil && (a.Part == "" || a.Part == "body") {
			c.ledger.Observe("material", a.MaterialID, out.Page)
		}
		return out, err
	})
	if err != nil {
		return nil, err
	}
	listMaterials, err := utils.InferTool("list_materials", "列出指定 Run 已保存的来源，阶段未结束也可查；后页同时传回 next_before_id、snapshot_max_id、query_key。", func(ctx context.Context, a struct {
		RunID int64 `json:"run_id"`
		personal.ListPage
	}) (personal.MaterialPage, error) {
		out, err := c.store.ListMaterials(ctx, c.scope, a.RunID, a.ListPage)
		if err != nil {
			return out, err
		}
		_, err = textpage.EncodeBounded(&out, textpage.MaxBytes(ctx), func() bool {
			if len(out.Items) <= 1 {
				return false
			}
			out.Items = out.Items[:len(out.Items)-1]
			out.HasMore = true
			out.NextBeforeID = out.Items[len(out.Items)-1].Seq
			return true
		})
		return out, err
	})
	if err != nil {
		return nil, err
	}
	search, err := utils.InferTool("search_material", "在已保存正文中做字面匹配，返回码点位置及短片段；搜索不代表已读全文。", func(ctx context.Context, a struct {
		MaterialID string `json:"material_id"`
		Query      string `json:"query"`
		personal.SearchPage
	}) (personal.MatchPage, error) {
		out, err := c.store.SearchMaterial(ctx, c.scope, a.MaterialID, a.Query, a.SearchPage)
		if err != nil {
			return out, err
		}
		_, err = textpage.EncodeBounded(&out, textpage.MaxBytes(ctx), func() bool {
			if len(out.Matches) <= 1 {
				return false
			}
			out.Matches = out.Matches[:len(out.Matches)-1]
			out.NextOffset = out.Matches[len(out.Matches)-1].Offset + 1
			out.HasMore = true
			return true
		})
		return out, err
	})
	if err != nil {
		return nil, err
	}
	readTask, err := utils.InferTool("read_task", "按页读取规范任务字段 goal/context/checkpoint；后页必须携带 version。", func(ctx context.Context, a struct {
		TaskID  int64  `json:"task_id"`
		Section string `json:"section"`
		textpage.Request
	}) (personal.RecordText, error) {
		return c.readRecord(ctx, personal.RecordRequest{Kind: "task", ID: a.TaskID, Section: a.Section, Request: a.Request})
	})
	if err != nil {
		return nil, err
	}
	readRun, err := utils.InferTool("read_run", "按页读取 Run 的 input/result/context_manifest JSON 文本；外部关联只允许 result。", func(ctx context.Context, a struct {
		RunID   int64  `json:"run_id"`
		Section string `json:"section"`
		textpage.Request
	}) (personal.RecordText, error) {
		return c.readRecord(ctx, personal.RecordRequest{Kind: "run", ID: a.RunID, Section: a.Section, Request: a.Request})
	})
	if err != nil {
		return nil, err
	}
	out := []tool.BaseTool{readMaterial, listMaterials, search, readTask, readRun}
	if c.scope.RunID > 0 {
		checkpoints, err := utils.InferTool("read_checkpoints", "按页读取当前 revision 的阅读位置和要点；后页携带 version，记录变化后从首页重新读取。", func(ctx context.Context, p textpage.Request) (textpage.Page, error) {
			values, err := c.store.ReadCheckpoints(ctx, c.scope.RunID)
			if err != nil {
				return textpage.Page{}, err
			}
			raw, err := json.Marshal(values)
			if err != nil {
				return textpage.Page{}, err
			}
			page, err := textpage.Read(string(raw), p, true)
			if err == nil {
				_, err = textpage.EncodeBounded(&page, textpage.MaxBytes(ctx), page.Shrink)
			}
			return page, err
		})
		if err != nil {
			return nil, err
		}
		out = append(out, checkpoints)
		checkpoint, err := utils.InferTool("checkpoint_reading", "记录连续读过的区间和提取要点；next_offset 不能跨过尚未返回的正文，note 必填。", func(ctx context.Context, u personal.ReadCheckpointUpdate) (personal.ReadCheckpoint, error) {
			if !c.ledger.covered(u) {
				return personal.ReadCheckpoint{}, errors.New("personal: checkpoint crosses unread text")
			}
			if err := c.validateResource(ctx, u); err != nil {
				return personal.ReadCheckpoint{}, err
			}
			return c.store.SaveReadCheckpoint(ctx, c.scope.RunID, u)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, checkpoint)
		listRuns, err := utils.InferTool("list_runs", "列出本 Task 的执行摘要；后页同时传回 next_before_id、snapshot_max_id、query_key。不会开放关联外部 Task 的整张列表。", func(ctx context.Context, a struct {
			TaskID int64 `json:"task_id"`
			personal.ListPage
		}) (personal.RunPage, error) {
			return c.store.QueryRuns(ctx, c.scope, a.TaskID, a.ListPage)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, listRuns)
		getRun, err := utils.InferTool("get_run", "查看本 Task 或明确关联 Run 的结果摘要；完整结果通过 read_run。", func(ctx context.Context, a struct {
			RunID int64 `json:"run_id"`
		}) (personal.Run, error) {
			if _, err := c.store.ReadRecord(ctx, c.scope, personal.RecordRequest{Kind: "run", ID: a.RunID, Section: "result", Request: textpage.Request{Limit: 1}}); err != nil {
				return personal.Run{}, err
			}
			r, err := c.store.Run(ctx, a.RunID)
			if err != nil {
				return personal.Run{}, err
			}
			if r == nil {
				return personal.Run{}, errors.New("run not found")
			}
			return personal.RunCard(*r), nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, getRun)
	} else {
		getEvent, err := utils.InferTool("get_event", "按 event_id 读取原始消息。", func(ctx context.Context, a struct {
			EventID int64 `json:"event_id"`
			textpage.Request
		}) (personal.RecordText, error) {
			return c.readRecord(ctx, personal.RecordRequest{Kind: "event", ID: a.EventID, Request: a.Request})
		})
		if err != nil {
			return nil, err
		}
		getMessage, err := utils.InferTool("get_message", "按 history_id 读取一条具体历史回复。", func(ctx context.Context, a struct {
			HistoryID int64 `json:"history_id"`
			textpage.Request
		}) (personal.RecordText, error) {
			return c.readRecord(ctx, personal.RecordRequest{Kind: "message", ID: a.HistoryID, Request: a.Request})
		})
		if err != nil {
			return nil, err
		}
		out = append(out, getEvent, getMessage)
	}
	return out, nil
}
func (c *readTools) readRecord(ctx context.Context, r personal.RecordRequest) (personal.RecordText, error) {
	out, err := c.store.ReadRecord(ctx, c.scope, r)
	if err == nil {
		_, err = textpage.EncodeBounded(&out, textpage.MaxBytes(ctx), out.Page.Shrink)
	}
	return out, err
}
func (c *readTools) validateResource(ctx context.Context, u personal.ReadCheckpointUpdate) error {
	switch u.ResourceKind {
	case "file":
		if c.readFile == nil {
			return errors.New("personal: workspace unavailable")
		}
		_, err := c.readFile(ctx, u.ResourceID, textpage.Request{Offset: u.NextOffset, Limit: 1, Version: u.Version})
		return err
	case "material":
		_, err := c.store.ReadMaterial(ctx, c.scope, u.ResourceID, "body", textpage.Request{Offset: u.NextOffset, Limit: 1, Version: u.Version})
		return err
	case "attachment":
		r, err := c.store.Run(ctx, c.scope.RunID)
		if err != nil {
			return err
		}
		if r == nil {
			return errors.New("personal: checkpoint run not found")
		}
		in, err := personal.DecodeTaskInput(r.Input)
		if err != nil {
			return err
		}
		allowed := c.attachments[u.ResourceID]
		for _, a := range in.Attachments {
			if a.ID == u.ResourceID {
				allowed = true
			}
		}
		if !allowed {
			return errors.New("personal: attachment not associated")
		}
		text, err := readAttachmentText(ctx, c.store, u.ResourceID)
		if err != nil {
			return err
		}
		_, err = textpage.Read(text, textpage.Request{Offset: u.NextOffset, Limit: 1, Version: u.Version}, false)
		return err
	default:
		return errors.New("personal: checkpoint resource not available in this tool scope")
	}
}

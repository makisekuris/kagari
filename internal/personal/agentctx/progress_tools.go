package agentctx

import (
	"context"
	"errors"
	"sync"
	"unicode/utf8"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"kagari/internal/personal"
	"kagari/internal/personal/internal/textclip"
	"kagari/internal/textpage"
)

type progressSubmissionReply struct {
	Submitted int    `json:"submitted"`
	Error     string `json:"error,omitempty"`
}

type ProgressRepository interface {
	personal.ProgressRepository
	Run(context.Context, int64) (*personal.Run, error)
}

type ProgressToolsConfig struct {
	Store              ProgressRepository
	Scope              personal.ReadScope
	Delta              *[]personal.ProgressUpdate
	AllowedAttachments map[string]bool
	CheckRun           func(context.Context, personal.Run) error
}

func ProgressTools(config ProgressToolsConfig) ([]tool.BaseTool, error) {
	if config.Store == nil || (config.Scope.RunID > 0 && (config.CheckRun == nil || config.Delta == nil)) {
		return nil, errors.New("personal: progress tools require a store and task run guard")
	}
	store, scope, delta, allowedAttachments := config.Store, config.Scope, config.Delta, config.AllowedAttachments
	get, err := utils.InferTool("get_progress", "按页查看累计进展；reported 是模型报告，非独立核实。保留旧 revision 的历史，不能当作新目标已完成。history=true 返回各版本记录。", func(ctx context.Context, a struct {
		TaskID int64 `json:"task_id"`
		personal.ProgressQuery
	}) (personal.TaskProgress, error) {
		page, err := store.TaskProgress(ctx, scope, a.TaskID, a.ProgressQuery)
		if err != nil {
			return personal.TaskProgress{}, err
		}
		return boundProgressPage(ctx, page)
	})
	if err != nil {
		return nil, err
	}
	read, err := utils.InferTool("read_progress", "按 task_id 和 seq 读取一条进展记录的完整 JSON 文本；用 offset、limit 和 version 续读。", func(ctx context.Context, a struct {
		TaskID int64 `json:"task_id"`
		Seq    int64 `json:"seq"`
		textpage.Request
	}) (personal.ProgressText, error) {
		return store.ReadProgress(ctx, scope, a.TaskID, a.Seq, a.Request)
	})
	if err != nil {
		return nil, err
	}
	out := []tool.BaseTool{get, read}
	if scope.RunID > 0 {
		var deltaMu sync.Mutex
		submit, err := utils.InferTool("submit_progress", "提交本阶段累计进展增量；同一 ID 的后一次调用覆盖本阶段前值，每阶段最多 50 项。只有阶段结果成功保存才生效。", func(ctx context.Context, a struct {
			Updates []personal.ProgressUpdate `json:"updates"`
		}) (progressSubmissionReply, error) {
			r, err := store.Run(ctx, scope.RunID)
			if err != nil {
				return progressSubmissionReply{}, err
			}
			if r == nil {
				return progressSubmissionReply{}, errors.New("run not found")
			}
			if err = config.CheckRun(ctx, *r); err != nil {
				return progressSubmissionReply{}, err
			}
			if err := personal.ValidateProgressUpdates(a.Updates); err != nil {
				return progressSubmissionReply{Error: err.Error()}, nil
			}

			deltaMu.Lock()
			defer deltaMu.Unlock()
			merged := append([]personal.ProgressUpdate(nil), (*delta)...)
			indices := make(map[string]int, len(merged)+len(a.Updates))
			for i, update := range merged {
				indices[update.ID] = i
			}
			for _, update := range a.Updates {
				if i, ok := indices[update.ID]; ok {
					merged[i] = update
					continue
				}
				indices[update.ID] = len(merged)
				merged = append(merged, update)
			}
			if len(merged) > 50 {
				return progressSubmissionReply{Error: personal.InvalidProgressError("at most 50 entries per stage").Error()}, nil
			}
			if err := store.ValidateProgressSubmission(ctx, *r, merged, allowedAttachments); err != nil {
				if errors.Is(err, personal.ErrInvalidProgress) {
					return progressSubmissionReply{Error: err.Error()}, nil
				}
				return progressSubmissionReply{}, err
			}
			*delta = merged
			return progressSubmissionReply{Submitted: len(a.Updates)}, nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, submit)
	} else {
		correct, err := utils.InferTool("correct_progress", "纠正累计进展，必须给出当前 Task revision 和进展 version；保留旧记录与修改原因。", func(ctx context.Context, a struct {
			TaskID          int64                     `json:"task_id"`
			Revision        int64                     `json:"revision"`
			ExpectedVersion int64                     `json:"expected_version"`
			Updates         []personal.ProgressUpdate `json:"updates"`
		}) (personal.TaskProgress, error) {
			page, err := store.CorrectProgress(ctx, scope.EventID, a.TaskID, a.Revision, a.ExpectedVersion, a.Updates)
			if err != nil {
				return personal.TaskProgress{}, err
			}
			return boundProgressPage(ctx, page)
		})
		if err != nil {
			return nil, err
		}
		out = append(out, correct)
	}
	return out, nil
}

func boundProgressPage(ctx context.Context, page personal.TaskProgress) (personal.TaskProgress, error) {
	for i := range page.Entries {
		entry := &page.Entries[i]
		if utf8.RuneCountInString(entry.Description) > 160 {
			entry.Description = textclip.Truncate(entry.Description, 160)
			entry.OmittedFields = appendProgressOmitted(entry.OmittedFields, "description")
		}
		if utf8.RuneCountInString(entry.Reason) > 120 {
			entry.Reason = textclip.Truncate(entry.Reason, 120)
			entry.OmittedFields = appendProgressOmitted(entry.OmittedFields, "reason")
		}
		if len(entry.Evidence) > 3 {
			entry.Evidence = entry.Evidence[:3]
			entry.OmittedFields = appendProgressOmitted(entry.OmittedFields, "evidence")
		}
	}
	_, err := textpage.EncodeBounded(&page, textpage.MaxBytes(ctx), func() bool {
		for i := range page.Entries {
			entry := &page.Entries[i]
			if utf8.RuneCountInString(entry.Description) > 20 {
				entry.Description = textclip.Truncate(entry.Description, utf8.RuneCountInString(entry.Description)/2)
				entry.OmittedFields = appendProgressOmitted(entry.OmittedFields, "description")
				return true
			}
			if utf8.RuneCountInString(entry.Reason) > 20 {
				entry.Reason = textclip.Truncate(entry.Reason, utf8.RuneCountInString(entry.Reason)/2)
				entry.OmittedFields = appendProgressOmitted(entry.OmittedFields, "reason")
				return true
			}
			if len(entry.Evidence) > 0 {
				if len(entry.Evidence) == 1 {
					entry.Evidence = nil
				} else {
					entry.Evidence = entry.Evidence[:len(entry.Evidence)/2]
				}
				entry.OmittedFields = appendProgressOmitted(entry.OmittedFields, "evidence")
				return true
			}
		}
		if len(page.Entries) > 1 {
			page.Entries = page.Entries[:len(page.Entries)-1]
			page.HasMore = true
			page.NextBeforeID = page.Entries[len(page.Entries)-1].Seq
			return true
		}
		return false
	})
	return page, err
}

func appendProgressOmitted(fields []string, field string) []string {
	for _, existing := range fields {
		if existing == field {
			return fields
		}
	}
	return append(fields, field)
}

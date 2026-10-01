package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/digest"
	"kagari/internal/domain"
	"kagari/internal/render"
	"kagari/internal/store"
)

type Worker struct {
	Store  *store.Store
	Engine *agent.Engine
	Config config.Config
	Log    *zap.Logger
}

func (w *Worker) Process(ctx context.Context, job *domain.Job) error {
	var result any
	var sources []domain.Source
	var text string
	var workErr error
	switch job.Kind {
	case "analyze":
		var s domain.Submission
		workErr = json.Unmarshal(job.Payload, &s)
		var r domain.Result
		if workErr == nil {
			cached, err := w.Store.FindResult(ctx, s.CacheKey)
			if err != nil {
				workErr = err
			} else if cached != nil && w.Config.Reader.CacheTTL > 0 && time.Since(cached.CreatedAt) < w.Config.Reader.CacheTTL {
				r = *cached
			} else {
				r, workErr = w.Engine.Analyze(ctx, s)
			}
		}
		result = r
		sources = r.Sources
		text = render.Analysis(r)
	case "digest":
		var request domain.DigestRequest
		workErr = json.Unmarshal(job.Payload, &request)
		if workErr == nil {
			entries, err := w.Store.Entries(ctx, request.UserID, request.Start, request.End)
			workErr = err
			if err == nil {
				total, pending, failed, err := w.Store.Stats(ctx, request.UserID, request.Start, request.End)
				workErr = err
				if err == nil {
					report := digest.Build(request.UserID, request.Start, request.End, time.Now(), entries, total, pending, failed)
					result = report
					text = digest.Render(report, w.Config.Weekly.Timezone)
				}
			}
		}
	case "command":
		var c domain.Command
		workErr = json.Unmarshal(job.Payload, &c)
		if workErr == nil {
			text = w.command(ctx, c)
			result = map[string]string{"reply": text}
		}
	case "notice":
		var c domain.Command
		workErr = json.Unmarshal(job.Payload, &c)
		text = c.Text
		result = map[string]string{"reply": text}
	default:
		workErr = errors.New("unknown job kind")
	}
	if result == nil {
		result = map[string]string{"status": "failed"}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if workErr != nil {
		// 取消任务时也先保存部分证据和失败状态；endpoint 的原始错误或响应正文不能写入日志。
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := w.Store.SaveAttempt(persistCtx, job.ID, raw); err != nil {
			return err
		}
		if err := w.Store.FailJob(persistCtx, job.ID, "processing failed; inspect archived source statuses or retry", w.Config.MaxAttempts, time.Now().Add(backoff(job.Attempts))); err != nil {
			return err
		}
		w.Log.Warn("job processing failed", zap.Int64("job_id", job.ID), zap.String("kind", job.Kind), zap.Int("attempt", job.Attempts))
		if job.Attempts >= w.Config.MaxAttempts {
			var owner domain.Submission
			_ = json.Unmarshal(job.Payload, &owner)
			if owner.ChatID != 0 {
				_, _, _ = w.notice(persistCtx, fmt.Sprintf("failed:%d", job.ID), owner.UserID, owner.ChatID, fmt.Sprintf("任务 #%d 处理失败，已保留读取记录。使用 /status %d 查看，/retry %d 重试。", job.ID, job.ID, job.ID))
			}
		}
		return workErr
	}
	var messages []string
	if job.TargetChatID != 0 {
		messages = render.Chunks(fmt.Sprintf("任务 #%d\n%s", job.ID, text))
	}
	return w.Store.CompleteJob(ctx, job.ID, raw, messages, sources)
}

func backoff(attempt int) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<attempt) * 5 * time.Second
}

func (w *Worker) EnqueueDigest(ctx context.Context, request domain.DigestRequest, target int64) (int64, bool, error) {
	if !request.Start.Before(request.End) {
		return 0, false, errors.New("start must be before end")
	}
	key := fmt.Sprintf("%d:%s:%s", request.UserID, request.Start.UTC().Format(time.RFC3339), request.End.UTC().Format(time.RFC3339))
	// 用户加完整周范围构成 period 幂等键；已完成周报视为快照，不因归档后来变化而重算。
	raw, _ := json.Marshal(request)
	return w.Store.Enqueue(ctx, "digest", key, raw, target)
}

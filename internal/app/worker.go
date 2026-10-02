package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/openai/openai-go/v3"
	"go.uber.org/zap"
	"kagari/internal/agent"
	"kagari/internal/config"
	"kagari/internal/domain"
	"kagari/internal/logging"
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
			} else if cached != nil && cached.AnalysisVersion == agent.AnalysisVersion && w.Config.Reader.CacheTTL > 0 && time.Since(cached.CreatedAt) < w.Config.Reader.CacheTTL {
				r = *cached
				w.Log.Info("agent analysis cache hit", zap.Int64("job_id", job.ID), zap.Int("attempt", job.Attempts), zap.Bool("model_called", false))
			} else {
				engine := *w.Engine
				engine.Log = w.Log.With(zap.Int64("job_id", job.ID), zap.Int("attempt", job.Attempts), zap.Int("max_attempts", w.Config.MaxAttempts))
				r, workErr = engine.Analyze(ctx, s)
			}
		}
		result = r
		sources = r.Sources
		text = render.Analysis(r)
	case "digest":
		result, text, workErr = w.processDigest(ctx, job)
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
		fields := []zap.Field{zap.Int64("job_id", job.ID), zap.String("kind", job.Kind), zap.Int("attempt", job.Attempts), zap.Int("max_attempts", w.Config.MaxAttempts)}
		if ctx.Err() != nil {
			if err := w.Store.InterruptJob(persistCtx, job.ID, raw); err != nil {
				return err
			}
			w.Log.Warn("job processing interrupted", append(fields, logging.ErrorFields(ctx.Err())...)...)
			return ctx.Err()
		}
		var apiErr *openai.Error
		if errors.As(workErr, &apiErr) {
			fields = append(fields, zap.String("stage", "model"))
		}
		fields = append(fields, logging.ErrorFields(workErr)...)
		terminal := job.Attempts >= w.Config.MaxAttempts
		var notice *domain.Command
		if terminal && (job.Kind == "analyze" || job.Kind == "digest") {
			var owner domain.Submission
			if json.Unmarshal(job.Payload, &owner) == nil {
				record := "读取记录"
				if job.Kind == "digest" && job.TargetChatID != 0 {
					owner.ChatID = owner.UserID
					record = "材料快照"
				}
				if owner.ChatID != 0 {
					notice = &domain.Command{UserID: owner.UserID, ChatID: owner.ChatID, Text: fmt.Sprintf("任务 #%d 已尝试 %d 次，仍未成功，已保留%s。使用 /status %d 查看，/retry %d 重试。", job.ID, job.Attempts, record, job.ID, job.ID)}
				}
			}
		}
		delay := backoff(job.Attempts)
		if err := w.Store.FailJob(persistCtx, job.ID, raw, logging.ErrorReason(workErr), w.Config.MaxAttempts, time.Now().Add(delay), notice); err != nil {
			return err
		}
		if terminal {
			w.Log.Error("job processing failed; attempts exhausted", fields...)
		} else {
			w.Log.Warn("job processing failed; retry scheduled", append(fields, zap.Duration("retry_after", delay))...)
		}
		return workErr
	}
	var messages []string
	if job.TargetChatID != 0 {
		if job.Kind == "digest" {
			messages = render.Chunks(text)
		} else {
			messages = render.Chunks(fmt.Sprintf("任务 #%d\n%s", job.ID, text))
		}
	}
	return w.Store.CompleteJob(ctx, job.ID, raw, messages, sources)
}

func backoff(attempt int) time.Duration {
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<attempt) * 5 * time.Second
}

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"
	"kagari/internal/agent"
	"kagari/internal/agent/prompt"
	"kagari/internal/digest"
	"kagari/internal/domain"
)

func (w *Worker) EnqueueDigest(ctx context.Context, request domain.DigestRequest, target int64) (int64, bool, error) {
	if !request.Start.Before(request.End) {
		return 0, false, errors.New("start must be before end")
	}
	if request.Version != "" && request.Version != domain.DigestVersion {
		return 0, false, errors.New("unsupported digest version")
	}
	request.Version = domain.DigestVersion
	// 版本、用户和精确时间范围共同组成任务键，同一请求只入队一次。
	key := fmt.Sprintf("%s:%d:%s:%s", request.Version, request.UserID, request.Start.UTC().Format(time.RFC3339Nano), request.End.UTC().Format(time.RFC3339Nano))
	raw, err := json.Marshal(request)
	if err != nil {
		return 0, false, err
	}
	return w.Store.Enqueue(ctx, "digest", key, raw, target)
}

func (w *Worker) processDigest(ctx context.Context, job *domain.Job) (report digest.Report, text string, err error) {
	var request domain.DigestRequest
	if err := json.Unmarshal(job.Payload, &request); err != nil {
		return report, "", err
	}
	if request.Version != "" && request.Version != domain.DigestVersion {
		return report, "", errors.New("unsupported digest version")
	}
	if len(job.Result) > 0 {
		if err := json.Unmarshal(job.Result, &report); err != nil {
			return report, "", fmt.Errorf("decode digest snapshot: %w", err)
		}
	}
	if report.Input == nil {
		entries, err := w.Store.Entries(ctx, request.UserID, request.Start, request.End)
		if err != nil {
			return report, "", err
		}
		profile := ""
		if w.Engine != nil {
			profile = w.Engine.Profile
		} else if len(entries) > 0 {
			data, err := os.ReadFile(w.Config.ProfilePath)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return report, "", fmt.Errorf("read profile: %w", err)
			}
			profile = string(data)
		}
		instruction := prompt.GetDigestPrompt("")
		if w.Engine != nil {
			instruction = w.Engine.DigestPrompt()
		} else if w.Persona != nil {
			instruction = prompt.GetDigestPrompt(w.Persona.Prompt())
		}
		now := time.Now().UTC()
		input, err := digest.Prepare(request, now, w.Config.Weekly.Timezone, profile, instruction, entries)
		if err != nil {
			return report, "", err
		}
		report = digest.Report{Version: domain.DigestVersion, UserID: request.UserID, Start: request.Start.UTC(), End: request.End.UTC(), GeneratedAt: now, Input: &input}
		if err := w.saveDigestProgress(ctx, job.ID, report); err != nil {
			return report, "", err
		}
	}
	input := report.Input
	if report.Version != domain.DigestVersion || input.UserID != request.UserID || !input.Start.Equal(request.Start) || !input.Cutoff.Equal(request.End) {
		return report, "", errors.New("digest snapshot does not match request")
	}
	if report.Review != nil {
		if err := digest.ValidateReview(*input, *report.Review); err != nil {
			return report, "", err
		}
		return report, digest.Render(report, input.Timezone), nil
	}
	if len(input.Entries) == 0 {
		report.Review = &domain.DigestReview{Sections: []domain.DigestSection{}}
	} else {
		raw, err := json.Marshal(input)
		if err != nil {
			return report, "", err
		}
		// ponytail: 以字符数限制输入大小；需要按 token 检查模型上下文上限时再接 tokenizer。
		if limit := w.Config.Weekly.InputLimit(); utf8.RuneCount(raw) > limit {
			return report, "", fmt.Errorf("digest input exceeds weekly.max_input_chars (%d)", limit)
		}
		engine, err := w.digestEngine(ctx, job)
		if err != nil {
			return report, "", err
		}
		review, usage, err := engine.GenerateDigest(ctx, *input)
		report.Usage.InputTokens += usage.InputTokens
		report.Usage.OutputTokens += usage.OutputTokens
		report.Usage.TotalTokens += usage.TotalTokens
		report.Model = engine.Config.Model.Name
		if err != nil {
			return report, "", err
		}
		report.Review = &review
	}
	report.GeneratedAt = time.Now().UTC()
	// 在 CompleteJob 前保存已校验正文；重启后回放它，再原子写入 outbox。
	if err := w.saveDigestProgress(ctx, job.ID, report); err != nil {
		return report, "", err
	}
	return report, digest.Render(report, input.Timezone), nil
}

func (w *Worker) saveDigestProgress(ctx context.Context, id int64, report digest.Report) error {
	raw, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return w.Store.SaveJobProgress(ctx, id, raw)
}

func (w *Worker) digestEngine(ctx context.Context, job *domain.Job) (*agent.Engine, error) {
	var engine agent.Engine
	if w.Engine != nil {
		engine = *w.Engine
	} else {
		cfg := w.Config
		cfg.ProfilePath = ""                               // 人格和偏好已冻结在输入内，恢复任务不重读偏好文件。
		created, err := agent.New(ctx, cfg, nil, nil, nil) // 完整提示词已冻结，不再读取当前人格。
		if err != nil {
			return nil, err
		}
		engine = *created
	}
	if w.Log != nil {
		engine.Log = w.Log.With(zap.Int64("job_id", job.ID), zap.Int("attempt", job.Attempts), zap.Int("max_attempts", w.Config.MaxAttempts))
	}
	return &engine, nil
}

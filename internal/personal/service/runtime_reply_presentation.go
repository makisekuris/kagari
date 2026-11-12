package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"kagari/internal/channel"
	"kagari/internal/logging"
	"kagari/internal/personal"
	"kagari/internal/reader"
)

// renderReply only changes the presentation of an already selected payload.
func (r *Runtime) renderReply(ctx context.Context, event personal.Event, output personal.TurnOutput) (personal.TurnOutput, []string, error) {
	if output.Text != "" {
		skipPresentation := false
		if event.Kind == personal.EventUser && output.ReplyKind == personal.ReplyReceipt {
			deferred, err := r.Store.CanDeferReply(ctx, event.ID)
			if err != nil {
				return output, nil, err
			}
			skipPresentation = deferred
		}
		if !skipPresentation {
			draft := output.Text
			presented, err := r.PresentReply(ctx, event, draft)
			if ctx.Err() != nil {
				return output, nil, ctx.Err()
			}
			if err == nil && strings.TrimSpace(presented) == "" {
				err = errors.New("reply presenter returned blank text")
			}
			if err == nil {
				draftURLs := reader.ExtractURLs(draft)
				presentedURLs := reader.ExtractURLs(presented)
				if len(presentedURLs) != len(draftURLs) {
					err = fmt.Errorf("reply presenter changed source URL set: draft has %d distinct URLs, presentation has %d", len(draftURLs), len(presentedURLs))
				} else {
					presentedURLSet := make(map[string]bool, len(presentedURLs))
					for _, url := range presentedURLs {
						presentedURLSet[url] = true
					}
					for _, url := range draftURLs {
						if !presentedURLSet[url] {
							err = fmt.Errorf("reply presenter omitted source URL %q", url)
							break
						}
					}
				}
			}
			if err != nil {
				logging.FromContext(ctx).Warn("conversation.reply.presentation_failed", append(logging.ErrorFields(err), zap.Int64("event_id", event.ID))...)
			} else {
				output.Text = presented
			}
			if ctx.Err() != nil {
				return output, nil, ctx.Err()
			}
		}
		var literals []string
		if event.Kind == personal.EventUser {
			literals = channel.MarkdownCodeLiterals(event.Text)
		}
		output.Text = channel.PlainTextMarkdown(output.Text, literals...)
	}

	if output.Text != "" {
		prefix, err := r.scheduleReplyPrefix(ctx, event)
		if err != nil {
			return output, nil, err
		}
		output.Text = prefix + output.Text
	}
	var chunks []string
	if output.Text != "" {
		var err error
		chunks, err = r.Prepare(output.Text)
		if err != nil {
			return output, nil, err
		}
	}
	if len(chunks) == 0 && len(output.Attachments) == 0 {
		return output, nil, errors.New("personal delivery produced no message")
	}

	return output, chunks, nil
}

func (r *Runtime) scheduleReplyPrefix(ctx context.Context, event personal.Event) (string, error) {
	prefix := ""
	if event.Kind == personal.EventTask && event.TaskID > 0 && event.ScheduleID > 0 {
		info, err := r.Store.ScheduleForTask(ctx, event.TaskID)
		if err != nil {
			return "", err
		}
		if info != nil {
			label := fmt.Sprintf("计划 #%d · 任务 #%d", info.Schedule.ID, event.TaskID)
			if w := info.Occurrence.Window; w != nil {
				if !info.Occurrence.CoverageEligible {
					label += fmt.Sprintf("\n原检查区间 [%s, %s)，本次目标已修改，不计入计划检查进度", w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339))
				} else {
					complete := info.Occurrence.CheckResult != nil && info.Occurrence.CheckResult.Complete && len(info.Occurrence.CheckResult.Limitations) == 0
					completeness := "不完整"
					if complete {
						completeness = "完整"
					}
					label += fmt.Sprintf("\n检查区间 [%s, %s)，检查结果：%s", w.Start.Format(time.RFC3339), w.End.Format(time.RFC3339), completeness)
				}
			}
			prefix = label + "\n\n"
		}
	}

	return prefix, nil
}

func (r *Runtime) unavailableAttachmentReply(ctx context.Context, event personal.Event) (string, []string, error) {
	prefix, err := r.scheduleReplyPrefix(ctx, event)
	if err != nil {
		return "", nil, err
	}
	text := prefix + "要发送的附件已删除或不可用，未发送。请重新上传或重新生成文件。"
	chunks, err := r.Prepare(text)
	return text, chunks, err
}

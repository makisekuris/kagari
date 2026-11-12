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
	"kagari/internal/personal/internal/textclip"
	"kagari/internal/reader"
)

func (r *Runtime) turn(ctx context.Context) (worked bool, turnErr error) {
	event, err := r.Store.ClaimEvent(ctx)
	if err != nil || event == nil {
		return false, err
	}
	log := r.eventLog(*event)
	ctx = logging.WithLogger(ctx, log)
	turnStarted := time.Now()
	outcome := "completed"
	log.Info("conversation.started", zap.Any("payload", event))
	defer func() {
		fields := []zap.Field{zap.Int64("elapsed_ms", time.Since(turnStarted).Milliseconds())}
		if turnErr != nil {
			log.Warn("conversation.failed", append(fields, logging.ErrorFields(turnErr)...)...)
		} else {
			log.Info("conversation.finished", append(fields, zap.String("outcome", outcome))...)
		}
	}()
	input, err := r.context(ctx, *event)
	if err != nil {
		return true, err
	}
	var task *personal.Task
	if event.Kind == personal.EventTask {
		task, err = r.Store.Task(ctx, event.TaskID)
		if err != nil {
			return true, err
		}
		if input.CurrentRun == nil || task == nil || input.CurrentRun.Revision != task.Revision || task.Status == personal.TaskCancelled {
			outcome = "stale"
			return true, r.Store.CompleteEvent(ctx, event.ID, personal.TurnOutput{}, nil)
		}
		if task.Status == personal.TaskWaitingBudget {
			return true, r.publish(ctx, *event, personal.TurnOutput{TaskID: task.ID, Text: taskBudgetText(task)})
		}
	}
	if event.Interrupted {
		outcome = "recovered"
		return true, r.recoverTurn(ctx, *event, task)
	}
	started := time.Now()
	turnCtx, cancel := context.WithCancel(ctx)
	if task != nil {
		cancel()
		remaining := time.Duration(task.BudgetMS-task.UsedMS) * time.Millisecond
		if remaining <= 0 {
			remaining = time.Millisecond
		}
		turnCtx, cancel = context.WithTimeout(ctx, remaining)
	}
	defer cancel()
	accountDone := make(chan error, 1)
	if task != nil {
		go func() {
			for wait(turnCtx, r.idleDelay) {
				if err := r.Store.AccountEvent(turnCtx, event.ID, time.Since(started)); err != nil {
					if turnCtx.Err() == nil {
						cancel()
						accountDone <- err
						return
					}
					break
				}
			}
			accountDone <- nil
		}()
	}
	output, thinkErr := r.Think(turnCtx, input)
	cancel()
	if task != nil {
		if err := <-accountDone; err != nil {
			return true, err
		}
		persistCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		err = r.Store.AccountEvent(persistCtx, event.ID, time.Since(started))
		stop()
		if err != nil {
			return true, err
		}
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	if thinkErr == nil && event.Kind == personal.EventUser && output.Silent {
		deferred, err := r.Store.CanDeferReply(ctx, event.ID)
		if err != nil {
			return true, err
		}
		if !deferred {
			if strings.TrimSpace(output.Text) != "" {
				output.Silent = false
			} else {
				thinkErr = errors.New("silent user response has no committed continuation")
			}
		}
	}
	if thinkErr != nil || (!output.Silent && strings.TrimSpace(output.Text) == "" && len(output.Attachments) == 0) {
		outcome = "fallback_reply"
		log.Warn("conversation.model.failed", logging.ErrorFields(thinkErr)...)
		fallback := "这轮回复未能完成。已提交的任务、计划和记忆操作仍保留，可以继续查询。"
		if errors.Is(thinkErr, personal.ErrAttachmentUnavailable) {
			fallback = "要发送的附件已删除或不可用，未发送。请重新上传或重新生成文件。"
		}
		output = personal.TurnOutput{Text: fallback, TaskID: event.TaskID, ScheduleID: event.ScheduleID}
		if event.Kind == personal.EventTask {
			output.Text = fmt.Sprintf("「%s」的阶段结果：\n%s", task.Title, textclip.Excerpt(event.Text, 6000))
		}
	}
	if task != nil {
		current, err := r.Store.Task(ctx, task.ID)
		if err != nil {
			return true, err
		}
		if current != nil && current.Status == personal.TaskAwaitingReview {
			pending, err := r.Store.PendingTaskInputs(ctx, task.ID)
			if err != nil {
				return true, err
			}
			if len(pending) > 0 {
				_, _, err = r.Store.ContinueTask(ctx, event.ID, "pending-inputs", task.ID, current.Revision, "Read the owner's additional material within the existing goal; build on the previous findings and report additions or corrections.", false)
				if err != nil {
					return true, err
				}
				output = personal.TurnOutput{TaskID: task.ID, Silent: true}
			} else {
				// An incomplete planning turn must not strand work without a visible state.
				settled, err := r.Store.SettleTask(ctx, event.ID, task.ID, current.Revision, personal.TaskWaitingInput, current.Summary)
				if err != nil {
					return true, err
				}
				output.Text = fmt.Sprintf("「%s」的阶段成果已保存，后续安排未能完成。你可以让我继续。\n\n%s", settled.Title, textclip.Excerpt(settled.Summary, 6000))
				output.TaskID = task.ID
				output.Silent = false
			}
		} else if current != nil && current.Status == personal.TaskWaitingBudget {
			output.Text = taskBudgetText(current)
			output.TaskID = task.ID
			output.Silent = false
		} else if current != nil && (current.Status == personal.TaskCompleted || current.Status == personal.TaskFailed || current.Status == personal.TaskWaitingInput) {
			output.Text = taskClosingText(current, personal.SummarizeRunResult(*input.CurrentRun).Sources)
			output.TaskID = task.ID
			output.Silent = false
		}
	}
	if errors.Is(thinkErr, personal.ErrAttachmentUnavailable) && !strings.Contains(output.Text, "要发送的附件已删除或不可用") {
		if output.Text != "" {
			output.Text += "\n\n"
		}
		output.Text += "要发送的附件已删除或不可用，未发送。请重新上传或重新生成文件。"
		output.Attachments = nil
	}
	if output.Silent {
		outcome = "silent"
	}
	return true, r.publish(ctx, *event, output)
}

// A restarted model could paraphrase an already committed tool call and create a second task.
func (r *Runtime) recoverTurn(ctx context.Context, event personal.Event, task *personal.Task) error {
	output := personal.TurnOutput{ScheduleID: event.ScheduleID, Text: "处理上一条消息时服务中断了。已提交的任务、计划和记忆操作仍保留，可以查询；尚未提交的部分请重新告诉我。"}
	if task != nil {
		if task.Status == personal.TaskQueued {
			return r.publish(ctx, event, personal.TurnOutput{TaskID: task.ID, ScheduleID: event.ScheduleID, Silent: true})
		}
		if task.Status == personal.TaskAwaitingReview {
			if _, err := r.Store.SettleTask(ctx, event.ID, task.ID, task.Revision, personal.TaskWaitingInput, task.Summary); err != nil {
				return err
			}
			output.Text = fmt.Sprintf("「%s」的阶段成果已保存，但后续安排被中断。请告诉我是否继续。\n\n%s", task.Title, textclip.Excerpt(task.Summary, 6000))
		} else {
			output.Text = fmt.Sprintf("「%s」此前的回复被中断，已保存的结果如下。\n\n%s", task.Title, textclip.Excerpt(task.Summary, 6000))
		}
		output.TaskID = task.ID
	}
	return r.publish(ctx, event, output)
}

func (r *Runtime) publish(ctx context.Context, event personal.Event, output personal.TurnOutput) error {
	if output.Silent {
		return r.Store.CompleteEvent(ctx, event.ID, personal.TurnOutput{TaskID: output.TaskID, ScheduleID: output.ScheduleID, Silent: true}, nil)
	}
	if output.Text != "" {
		skipPresentation := false
		if event.Kind == personal.EventUser && output.ReplyKind == personal.ReplyReceipt {
			deferred, err := r.Store.CanDeferReply(ctx, event.ID)
			if err != nil {
				return err
			}
			skipPresentation = deferred
		}
		if !skipPresentation {
			draft := output.Text
			presented, err := r.PresentReply(ctx, event, draft)
			if ctx.Err() != nil {
				return ctx.Err()
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
				return ctx.Err()
			}
		}
		var literals []string
		if event.Kind == personal.EventUser {
			literals = channel.MarkdownCodeLiterals(event.Text)
		}
		output.Text = channel.PlainTextMarkdown(output.Text, literals...)
	}
	prefix := ""
	if event.Kind == personal.EventTask && event.TaskID > 0 && event.ScheduleID > 0 {
		info, err := r.Store.ScheduleForTask(ctx, event.TaskID)
		if err != nil {
			return err
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
			output.Text = prefix + output.Text
			output.ScheduleID = event.ScheduleID
		}
	}
	var chunks []string
	if output.Text != "" {
		var err error
		chunks, err = r.Prepare(output.Text)
		if err != nil {
			return err
		}
	}
	if len(chunks) == 0 && len(output.Attachments) == 0 {
		return errors.New("personal delivery produced no message")
	}
	if err := r.Store.CompleteEvent(ctx, event.ID, output, chunks); err != nil {
		if !errors.Is(err, personal.ErrAttachmentUnavailable) || len(output.Attachments) == 0 {
			return err
		}
		fallback := personal.TurnOutput{TaskID: output.TaskID, ScheduleID: output.ScheduleID, Text: prefix + "要发送的附件已删除或不可用，未发送。请重新上传或重新生成文件。"}
		fallbackChunks, prepareErr := r.Prepare(fallback.Text)
		if prepareErr != nil {
			return prepareErr
		}
		return r.Store.CompleteEvent(ctx, event.ID, fallback, fallbackChunks)
	}
	return nil
}

func (r *Runtime) context(ctx context.Context, event personal.Event) (personal.TurnInput, error) {
	input := personal.TurnInput{Event: event}
	var err error
	if event.Kind == personal.EventUser {
		input.PendingInputs, err = r.Store.PendingInputs(ctx, event.ID)
		if err != nil {
			return input, err
		}
	}
	if event.Kind == personal.EventTask {
		input.PendingTaskInputs, err = r.Store.PendingTaskInputs(ctx, event.TaskID)
		if err != nil {
			return input, err
		}
	}
	input.History, err = r.Store.History(ctx, event.ID, 20)
	if err != nil {
		return input, err
	}
	input.Tasks, err = r.Store.Tasks(ctx, 10)
	if err != nil {
		return input, err
	}
	if event.TaskID == 0 && event.ReplyToID != "" {
		referenced, err := r.Store.ReferencedTasks(ctx, event.ID)
		if err != nil {
			return input, err
		}
		for _, task := range referenced {
			input.ReferencedTaskIDs = append(input.ReferencedTaskIDs, task.ID)
			found := false
			for _, existing := range input.Tasks {
				if existing.ID == task.ID {
					found = true
					break
				}
			}
			if !found {
				input.Tasks = append(input.Tasks, task)
			}
		}
	}
	if event.RunID != 0 {
		input.CurrentRun, err = r.Store.Run(ctx, event.RunID)
		if err != nil {
			return input, err
		}
	}
	if event.TaskID != 0 {
		found := false
		for _, task := range input.Tasks {
			if task.ID == event.TaskID {
				found = true
			}
		}
		if !found {
			task, err := r.Store.Task(ctx, event.TaskID)
			if err != nil {
				return input, err
			}
			if task != nil {
				input.Tasks = append([]personal.Task{*task}, input.Tasks...)
			}
		}
	}
	if event.TaskID > 0 {
		input.Schedule, err = r.Store.ScheduleForTask(ctx, event.TaskID)
		if err != nil {
			return input, err
		}
	}
	if input.Schedule != nil {
		input.Suggestions, err = r.Store.ScheduleSuggestions(ctx, input.Schedule.Schedule.ID)
		if err != nil {
			return input, err
		}
	} else if event.ScheduleID > 0 {
		input.Suggestions, err = r.Store.ScheduleSuggestions(ctx, event.ScheduleID)
		if err != nil {
			return input, err
		}
	}
	if event.ReplyOutboxID > 0 {
		input.RepliedMessage, err = r.Store.RepliedMessage(ctx, event.ReplyOutboxID)
		if err != nil {
			return input, err
		}
		replied, err := r.Store.ReplyScheduleSuggestions(ctx, event.ReplyOutboxID)
		if err != nil {
			return input, err
		}
		input.ReplySuggestions = replied
	}
	input.Memories, err = r.Store.Memories(ctx, "", 20)
	if err != nil {
		return input, err
	}
	return input, nil
}
